package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// CandidateInput 是一条待写入的候选。
//
// 与 chain.NewPair 的区别:这里已经**选定了**哪一侧是新币
// (由 chain.FilterQuoteTokens 按出现频率判定),不再保留两侧。
type CandidateInput struct {
	Token  string
	Pair   string
	Block  uint64
	TxHash string
}

// SavePairs 批量写入链上扫到的建单记录。
//
// 用 INSERT IGNORE + 唯一键去重,所以同一个区间重复扫、或者 24 小时那轮
// 跑完再跑 48 小时那轮,都不会产生重复行——这是"先跑 24h 再跑 48h"
// 这个用法能成立的前提。
func (s *Store) SavePairs(ctx context.Context, pairs []CandidateInput) (int64, error) {
	if len(pairs) == 0 {
		return 0, nil
	}

	const (
		columns = `contract_address, pair_address, pool_block, pool_tx`
		batch   = 500
	)

	var affected int64
	for start := 0; start < len(pairs); start += batch {
		end := start + batch
		if end > len(pairs) {
			end = len(pairs)
		}
		chunk := pairs[start:end]

		row := "(?, ?, ?, ?)"
		placeholders := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*4)
		for _, p := range chunk {
			placeholders = append(placeholders, row)
			args = append(args, p.Token, nullIfEmpty(p.Pair), p.Block, nullIfEmpty(p.TxHash))
		}

		query := "INSERT IGNORE INTO chain_candidates (" + columns + ") VALUES " +
			strings.Join(placeholders, ",")

		n, err := execWithRetry(ctx, s.db, query, args)
		if err != nil {
			// 单批失败**不中止整轮**。这是长任务:一次连接抖动就全盘放弃,
			// 代价远大于跳过 500 条(下次重跑会自动补上——有唯一键去重)。
			continue
		}
		affected += n
	}
	return affected, nil
}

// execWithRetry 执行一条写语句,遇到瞬时连接故障时重试。
//
// 需要它的原因很实际:长时间批量写会撞上服务端主动断开
// (实测报 "invalid connection"),而 database/sql 的自动重试并不覆盖
// 已经进入驱动层的这种情况。
func execWithRetry(ctx context.Context, db *sql.DB, query string, args []any) (int64, error) {
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-time.After(time.Duration(1<<uint(i-1)) * 500 * time.Millisecond):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		res, err := db.ExecContext(ctx, query, args...)
		if err == nil {
			n, _ := res.RowsAffected()
			return n, nil
		}
		lastErr = err
	}
	return 0, lastErr
}

// Candidate 是一条待核验的代币。
type Candidate struct {
	ContractAddress string
	PairAddress     string
	PoolBlock       uint64
}

// PendingCandidates 取出还没问过 GMGN 的候选。
//
// 按建池区块倒序:越新的币越可能还活跃,先跑先出结果。
//
// limit <= 0 表示不限量。这一点必须显式处理——拼进 SQL 会变成 LIMIT 0,
// 也就是"一条都不返回",而调用方看到的现象是"本轮核验 0 个"却没有任何报错。
func (s *Store) PendingCandidates(ctx context.Context, limit int) ([]Candidate, error) {
	query := `
		SELECT contract_address, COALESCE(pair_address, ''), COALESCE(pool_block, 0)
		FROM chain_candidates
		WHERE checked_at IS NULL
		   OR (verdict = 'error' AND checked_at < NOW() - INTERVAL 1 HOUR)
		ORDER BY pool_block DESC`

	args := []any{}
	if limit > 0 {
		query += "\n\t\tLIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		var block int64
		if err := rows.Scan(&c.ContractAddress, &c.PairAddress, &block); err != nil {
			return nil, err
		}
		if block > 0 {
			c.PoolBlock = uint64(block)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CandidateVerdict 是核验结论。
type CandidateVerdict string

const (
	// VerdictPassed 通过全部条件,已写入 token_screen_metrics。
	VerdictPassed CandidateVerdict = "passed"
	// VerdictRejected 被条件筛掉。
	VerdictRejected CandidateVerdict = "rejected"
	// VerdictNoData GMGN 没有这个币的数据。
	VerdictNoData CandidateVerdict = "nodata"
	// VerdictNoLiquidity 在 DexScreener 上查不到交易对,或流动性低到等于空池。
	//
	// 单独一类而不是并进 rejected:它代表"被免费的预筛拦下,一次 GMGN 都没调",
	// 与"问过 GMGN 后被条件筛掉"是两回事。混在一起就看不出预筛到底省了多少。
	VerdictNoLiquidity CandidateVerdict = "nolp"
	// VerdictError 调用失败。这类会被重新拾取重试。
	VerdictError CandidateVerdict = "error"
)

// MarkCandidates 批量标记核验结论。
func (s *Store) MarkCandidates(ctx context.Context, addrs []string, v CandidateVerdict) error {
	if len(addrs) == 0 {
		return nil
	}
	const batch = 500
	for start := 0; start < len(addrs); start += batch {
		end := start + batch
		if end > len(addrs) {
			end = len(addrs)
		}
		chunk := addrs[start:end]

		ph := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, string(v))
		for i, a := range chunk {
			ph[i] = "?"
			args = append(args, a)
		}
		query := "UPDATE chain_candidates SET checked_at = NOW(), verdict = ? WHERE contract_address IN (" +
			strings.Join(ph, ",") + ")"
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("标记 %d 条候选失败: %w", len(chunk), err)
		}
	}
	return nil
}

// CandidateStats 是候选池的进度统计。
type CandidateStats struct {
	Total       int64
	Pending     int64
	Passed      int64
	Rejected    int64
	NoData      int64
	NoLiquidity int64
	Errored     int64
	NewestAt    time.Time
	OldestAt    time.Time
}

// CandidateProgress 统计候选池进度,用于在长任务里打印进度。
func (s *Store) CandidateProgress(ctx context.Context) (*CandidateStats, error) {
	const query = `
		SELECT
			COUNT(*)                                              AS total,
			SUM(checked_at IS NULL)                               AS pending,
			SUM(verdict = 'passed')                               AS passed,
			SUM(verdict = 'rejected')                             AS rejected,
			SUM(verdict = 'nodata')                               AS nodata,
			SUM(verdict = 'nolp')                                 AS nolp,
			SUM(verdict = 'error')                                AS errored,
			COALESCE(MIN(discovered_at), NOW())                   AS oldest,
			COALESCE(MAX(discovered_at), NOW())                   AS newest
		FROM chain_candidates`

	var st CandidateStats
	var pending, passed, rejected, nodata, nolp, errored sql.NullInt64
	err := s.db.QueryRowContext(ctx, query).Scan(
		&st.Total, &pending, &passed, &rejected, &nodata, &nolp, &errored, &st.OldestAt, &st.NewestAt)
	if err != nil {
		return nil, err
	}
	st.Pending = pending.Int64
	st.Passed = passed.Int64
	st.Rejected = rejected.Int64
	st.NoData = nodata.Int64
	st.NoLiquidity = nolp.Int64
	st.Errored = errored.Int64
	return &st, nil
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

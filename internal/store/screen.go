package store

import (
	"context"
	"database/sql"
	"strings"

	"stock/internal/model"
)

// screenColumns 与 screenArgs 的字段顺序严格对应。
const screenColumns = `contract_address, name, symbol, source, launched_at,
	holder_count, sniper_count, market_cap, liquidity,
	launchpad_status, launchpad_progress, migration_market_cap,
	creator_address, creator_token_status,
	sniper_rate, sniper_rate_basis, sniper_count_rate, sniper_hold_rate, bundler_rate,
	top10_rate, top10_rate_no_lp, top10_max_rate, top10_count,
	lp_excluded_count, lp_excluded_rate, lp_basis, holder_coverage,
	creator_rate, is_honeypot, buy_tax, sell_tax, is_open_source, is_renounced,
	criteria_json, holders_json`

const screenColumnCount = 35

// screenUpsertClause 在冲突时整体覆盖。
//
// 与 meme_tokens 的 upsert 不同,这里**全部字段都覆盖**:本表存的是"最新一次
// 判定的快照",没有哪个字段是"首次发现才权威"的。checked_at 由 DB 自动更新。
const screenUpsertClause = `
ON DUPLICATE KEY UPDATE
	name = VALUES(name),
	symbol = VALUES(symbol),
	source = VALUES(source),
	launched_at = VALUES(launched_at),
	holder_count = VALUES(holder_count),
	sniper_count = VALUES(sniper_count),
	market_cap = VALUES(market_cap),
	liquidity = VALUES(liquidity),
	launchpad_status = VALUES(launchpad_status),
	launchpad_progress = VALUES(launchpad_progress),
	migration_market_cap = VALUES(migration_market_cap),
	creator_address = VALUES(creator_address),
	creator_token_status = VALUES(creator_token_status),
	sniper_rate = VALUES(sniper_rate),
	sniper_rate_basis = VALUES(sniper_rate_basis),
	sniper_count_rate = VALUES(sniper_count_rate),
	sniper_hold_rate = VALUES(sniper_hold_rate),
	bundler_rate = VALUES(bundler_rate),
	top10_rate = VALUES(top10_rate),
	top10_rate_no_lp = VALUES(top10_rate_no_lp),
	top10_max_rate = VALUES(top10_max_rate),
	top10_count = VALUES(top10_count),
	lp_excluded_count = VALUES(lp_excluded_count),
	lp_excluded_rate = VALUES(lp_excluded_rate),
	lp_basis = VALUES(lp_basis),
	holder_coverage = VALUES(holder_coverage),
	creator_rate = VALUES(creator_rate),
	is_honeypot = VALUES(is_honeypot),
	buy_tax = VALUES(buy_tax),
	sell_tax = VALUES(sell_tax),
	is_open_source = VALUES(is_open_source),
	is_renounced = VALUES(is_renounced),
	criteria_json = VALUES(criteria_json),
	holders_json = VALUES(holders_json)`

// SaveScreenMetrics 写入一条筛选通过的记录。
//
// 靠 uk_contract 唯一键实现"每个合约地址只保留最新一条"。
func (s *Store) SaveScreenMetrics(ctx context.Context, r model.ScreenRecord) error {
	row := "(" + strings.TrimSuffix(strings.Repeat("?,", screenColumnCount), ",") + ")"
	query := "INSERT INTO token_screen_metrics (" + screenColumns + ") VALUES " + row + screenUpsertClause

	_, err := s.db.ExecContext(ctx, query, screenArgs(r)...)
	return err
}

// screenArgs 按 screenColumns 的顺序展开字段。
func screenArgs(r model.ScreenRecord) []any {
	var source any
	if r.Source != "" {
		source = string(r.Source)
	}
	return []any{
		r.ContractAddress, r.Name, r.Symbol, source, r.LaunchedAt,
		r.HolderCount, r.SniperCount, r.MarketCap, r.Liquidity,
		r.LaunchpadStatus, r.LaunchpadProgress, r.MigrationMarketCap,
		nullString(r.CreatorAddress), nullString(r.CreatorTokenStatus),
		r.SniperRate, r.SniperRateBasis, r.SniperCountRate, r.SniperHoldRate, r.BundlerRate,
		r.Top10Rate, r.Top10RateNoLP, r.Top10MaxRate, r.Top10Count,
		r.LPExcludedCount, r.LPExcludedRate, lpBasisOrNone(r.LPBasis), r.HolderCoverage,
		r.CreatorRate, r.IsHoneypot, r.BuyTax, r.SellTax, r.IsOpenSource, r.IsRenounced,
		nullableJSON(r.CriteriaJSON), nullableJSON(r.HoldersJSON),
	}
}

// nullString 把空字符串写成 NULL。
//
// 空串与 NULL 在语义上不同:前者是"确实取到了空值",后者是"没有这个信息"。
// 而 meme_tokens.pair_address 就是因为写进了空串,导致 `IS NOT NULL` 判断失真。
func nullString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// lpBasisOrNone 把空依据统一成 "none"。
//
// 与"识别到了但依据为空"区分开:空串在报表里看不出是哪种情况。
func lpBasisOrNone(basis string) string {
	if strings.TrimSpace(basis) == "" {
		return "none"
	}
	return basis
}

// nullableJSON 把空字符串当作 NULL 写入。
//
// 空串不是合法 JSON,直接写进 JSON 列会被 MySQL 拒绝;而且"没有明细"
// 与"明细是空数组"在语义上确实不同。
func nullableJSON(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// ListScreenResults 取通过筛选的代币,按需求关心度排序。
//
// 排序口径:市值升序(越小越"便宜"),同市值按新近程度。
func (s *Store) ListScreenResults(ctx context.Context, limit int) ([]model.ScreenRecord, error) {
	const query = `
		SELECT contract_address, name, symbol, launched_at,
		       holder_count, sniper_count, market_cap, liquidity,
		       sniper_rate, top10_max_rate, top10_rate_no_lp,
		       lp_excluded_count, lp_basis, checked_at
		FROM token_screen_metrics
		ORDER BY market_cap ASC, checked_at DESC
		LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.ScreenRecord
	for rows.Next() {
		var (
			r         model.ScreenRecord
			name      sql.NullString
			symbol    sql.NullString
			holders   sql.NullInt64
			snipers   sql.NullInt64
			mcap      sql.NullFloat64
			liq       sql.NullFloat64
			sniperRt  sql.NullFloat64
			top10Max  sql.NullFloat64
			top10NoLP sql.NullFloat64
			lpCount   sql.NullInt64
			lpBasis   sql.NullString
			checkedAt sql.NullTime
		)
		if err := rows.Scan(&r.ContractAddress, &name, &symbol, &r.LaunchedAt,
			&holders, &snipers, &mcap, &liq,
			&sniperRt, &top10Max, &top10NoLP,
			&lpCount, &lpBasis, &checkedAt); err != nil {
			return nil, err
		}
		r.Name = name.String
		r.Symbol = symbol.String
		r.HolderCount = holders.Int64
		r.SniperCount = snipers.Int64
		r.MarketCap = mcap.Float64
		r.Liquidity = liq.Float64
		r.SniperRate = sniperRt.Float64
		r.Top10MaxRate = top10Max.Float64
		r.Top10RateNoLP = top10NoLP.Float64
		r.LPExcludedCount = int(lpCount.Int64)
		r.LPBasis = lpBasis.String
		r.CheckedAt = checkedAt.Time
		out = append(out, r)
	}
	return out, rows.Err()
}

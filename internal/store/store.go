// Package store 封装 MySQL 访问。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Store 是数据库访问入口,并发安全。
type Store struct {
	db *sql.DB
}

// New 建立连接池并验证连通性。
func New(dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	// 连接寿命必须显著短于 MySQL 的 wait_timeout(默认 8 小时,但实测 phpStudy
	// 的 5.7 会提前断开空闲连接)。旧连接被服务端单方面关掉之后,若客户端仍
	// 以为它可用,写操作就会挂在 TCP 上——实测表现是进程活着、CPU 不涨、日志
	// 不动,像卡死,重试逻辑根本等不到错误返回。3 分钟远短于任何合理的
	// wait_timeout,代价只是偶尔多建一次连接。
	db.SetConnMaxLifetime(3 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭连接池。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接,供需要自定义查询的调用方使用。
func (s *Store) DB() *sql.DB { return s.db }

// Migrate 执行单个建表脚本。
//
// 脚本可重复执行(表定义都带 IF NOT EXISTS)。
func (s *Store) Migrate(ctx context.Context, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取迁移文件 %s 失败: %w", path, err)
	}

	stmts := splitStatements(string(raw))
	for i, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行 %s 第 %d 条语句失败: %w\n语句: %s",
				filepath.Base(path), i+1, err, truncate(stmt, 200))
		}
	}
	return nil
}

// MigrateDir 按文件名字典序执行目录下的全部 .sql 脚本。
//
// 刻意不引入版本追踪表:文件名的零填充前缀就是执行顺序,而语句全部幂等,
// 使"重复执行"等价于"已是最新"。
//
// 代价是**已发布的 .sql 不可再修改**——迁移没有校验和,改动不会被重放,
// 只能新增一个文件。这也要求新文件名继续用零填充前缀,否则字典序会与
// 数字序脱钩(字符串比较下 "010" < "002")。
//
// 注意:目录里**保留什么文件就等于保留什么表**。删掉某个 .sql 并不会删表,
// 但表被删之后再跑一次,少掉的那个就不会被重建——这正是我们想要的。
func (s *Store) MigrateDir(ctx context.Context, dir string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return fmt.Errorf("查找迁移文件失败: %w", err)
	}
	// 目录写错时静默什么都不做是最坏的失败模式:后续 SQL 会以"表不存在"的形式
	// 报错,离真正的原因很远。这里直接失败。
	if len(paths) == 0 {
		return fmt.Errorf("目录 %s 下没有 .sql 迁移文件", dir)
	}

	sort.Strings(paths)
	for _, p := range paths {
		if err := s.Migrate(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// splitStatements 把迁移脚本切成独立语句。
//
// go-sql-driver 默认不允许多语句执行,而开启 multiStatements 会放宽整个连接
// 的安全边界,对一个可能拼接字符串的项目来说不划算。这里做一个够用的解析:
// 逐行读取,跳过 `--` 注释与空行,按行尾分号切分(本项目的脚本中分号只出现
// 在语句末尾)。
//
// 因此迁移文件有两条格式硬约束:
//  1. 语句结尾的 ';' 必须独占行尾——写成 "x; -- 注释" 会导致切分点识别不到
//  2. 注释必须独占一行
func splitStatements(sqlText string) []string {
	var (
		stmts []string
		buf   strings.Builder
	)
	for _, line := range strings.Split(sqlText, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			if s := strings.TrimSpace(buf.String()); s != "" {
				stmts = append(stmts, strings.TrimSuffix(s, ";"))
			}
			buf.Reset()
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

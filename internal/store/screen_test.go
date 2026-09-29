package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"stock/internal/model"
)

// splitColumns 把逗号分隔的列清单切成列名。
//
// 同时处理跨行的写法:screenColumns 是多行字符串,每行以逗号结尾。
func splitColumns(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TestScreenColumnArity 保证列清单、占位符个数、参数个数三者严格一致。
//
// 三者不一致只会在真正执行 INSERT 时才炸,报错是驱动的
// "Column count doesn't match value count"——离"我加了列却忘了改参数"
// 这个真正的原因很远。这条测试把它变成编译后立刻可见的失败。
func TestScreenColumnArity(t *testing.T) {
	cols := splitColumns(screenColumns)
	if len(cols) != screenColumnCount {
		t.Errorf("screenColumns 切出 %d 列,但 screenColumnCount = %d", len(cols), screenColumnCount)
	}

	args := screenArgs(model.ScreenRecord{})
	if len(args) != screenColumnCount {
		t.Errorf("screenArgs 返回 %d 个参数,期望 %d 个", len(args), screenColumnCount)
	}

	row := "(" + strings.TrimSuffix(strings.Repeat("?,", screenColumnCount), ",") + ")"
	if n := strings.Count(row, "?"); n != screenColumnCount {
		t.Errorf("VALUES 占位符 %d 个,期望 %d 个", n, screenColumnCount)
	}
}

// TestScreenColumnsExistInMigration 保证 INSERT 用到的每一列在建表脚本里都有定义。
//
// 漏一列的后果是运行时的 "Unknown column" —— 同样要等到真跑才暴露,
// 而那时已经把 GMGN 配额花掉了。
func TestScreenColumnsExistInMigration(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(migrationsDir, "001_screening.sql"))
	if err != nil {
		t.Fatalf("读取迁移文件失败: %v", err)
	}
	sql := string(raw)

	for _, col := range splitColumns(screenColumns) {
		// 列定义形如 "  holder_count       INT UNSIGNED  DEFAULT NULL,"
		re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(col) + `\s+[A-Za-z]`)
		if !re.MatchString(sql) {
			t.Errorf("列 %q 在 001_screening.sql 里没有定义", col)
		}
	}
}

// TestScreenUpsertClauseUsesKnownColumns 保证 upsert 更新的字段都在列清单内。
//
// 拼错的字段名会在 ON DUPLICATE KEY UPDATE 里变成一个未被引用的标识符,
// MySQL 直接报语法错误。
func TestScreenUpsertClauseUsesKnownColumns(t *testing.T) {
	known := map[string]bool{}
	for _, c := range splitColumns(screenColumns) {
		known[c] = true
	}

	// 形如 "	name = VALUES(name),"
	re := regexp.MustCompile(`(?m)^\s*([a-z_0-9]+)\s*=\s*VALUES\(`)
	matches := re.FindAllStringSubmatch(screenUpsertClause, -1)
	if len(matches) == 0 {
		t.Fatalf("没有从 upsert 子句里解析出任何字段,正则可能失效了")
	}
	for _, m := range matches {
		if !known[m[1]] {
			t.Errorf("upsert 更新了不在列清单里的字段 %q", m[1])
		}
	}

	// 唯一键列不应出现在 UPDATE 里——它是冲突判定依据,改了毫无意义
	if strings.Contains(screenUpsertClause, "contract_address") {
		t.Errorf("upsert 不应更新唯一键 contract_address")
	}
}

// TestLpBasisOrNone 保证空依据被归一化成 "none" 而不是空串。
//
// 空串在报表里看不出是"没识别到 LP"还是"识别到了但依据没记上"。
func TestLpBasisOrNone(t *testing.T) {
	if got := lpBasisOrNone(""); got != "none" {
		t.Errorf("空串应归一化为 none,实际 %q", got)
	}
	if got := lpBasisOrNone("   "); got != "none" {
		t.Errorf("纯空白应归一化为 none,实际 %q", got)
	}
	if got := lpBasisOrNone("addr_type_pool"); got != "addr_type_pool" {
		t.Errorf("非空值不应被改动,实际 %q", got)
	}
}

// TestNullableJSON 保证空串以 NULL 写入。
//
// 空串不是合法 JSON,直接送进 JSON 列会被 MySQL 拒绝;而且"没有明细"
// 与"明细是空数组"在语义上确实不同。
func TestNullableJSON(t *testing.T) {
	if got := nullableJSON(""); got != nil {
		t.Errorf("空串应转为 NULL,实际 %v", got)
	}
	if got := nullableJSON("  "); got != nil {
		t.Errorf("纯空白应转为 NULL,实际 %v", got)
	}
	if got := nullableJSON("[]"); got != "[]" {
		t.Errorf("合法 JSON 应原样保留,实际 %v", got)
	}
}

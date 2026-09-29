package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// migrationsDir 是相对本测试包的迁移目录。
const migrationsDir = "../../migrations"

// TestSplitStatementsOnRealMigrations 拿真实的迁移文件验证切分逻辑。
//
// splitStatements 的规则很脆:它逐行读取、要求 ';' 独占行尾(前一条语句的
// "x; -- 注释" 会让 HasSuffix(trimmed,";") 为假,把两条语句粘成一条)。
// 这类错误在写 SQL 时毫无感觉,只有在执行时才以"语法错误"的形式暴露,
// 而那时人已经在别处找原因了。所以直接对真实文件断言。
func TestSplitStatementsOnRealMigrations(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		t.Fatalf("查找迁移文件失败: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("%s 下没有迁移文件", migrationsDir)
	}

	for _, p := range paths {
		name := filepath.Base(p)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}

			stmts := splitStatements(string(raw))
			if len(stmts) == 0 {
				t.Fatalf("没有切出任何语句")
			}

			for i, stmt := range stmts {
				// 每条语句应当且只应当是一条建表语句。
				// 出现两次 CREATE TABLE 说明两条被粘在了一起。
				if n := strings.Count(stmt, "CREATE TABLE"); n != 1 {
					t.Errorf("第 %d 条语句含 %d 个 CREATE TABLE,应为 1——切分点可能漏了\n语句开头: %s",
						i+1, n, truncate(stmt, 160))
				}
				if !strings.HasPrefix(strings.ToUpper(stmt), "CREATE TABLE") {
					t.Errorf("第 %d 条语句不是以 CREATE TABLE 开头: %s",
						i+1, truncate(stmt, 160))
				}
				// 建表语句的最后一个非空字符应当是右括号
				if !strings.HasSuffix(strings.TrimSpace(stmt), ")") &&
					!strings.Contains(stmt, "COMMENT=") {
					t.Errorf("第 %d 条语句结尾可疑: %s", i+1, truncate(stmt, 160))
				}
				// 残留的分号说明 TrimSuffix 没生效,交给驱动会报语法错误
				if strings.HasSuffix(strings.TrimSpace(stmt), ";") {
					t.Errorf("第 %d 条语句残留分号", i+1)
				}
				// 注释行应当已被跳过
				if strings.Contains(stmt, "--") {
					t.Errorf("第 %d 条语句里混进了注释: %s", i+1, truncate(stmt, 160))
				}
			}
		})
	}
}

// TestSplitStatementsGolden 用一段构造出的 SQL 钉住切分规则本身。
func TestSplitStatementsGolden(t *testing.T) {
	const src = `
-- 文件头注释
-- 第二行注释

CREATE TABLE a (
  id INT,
  x  VARCHAR(10)  -- 行内注释也在语句里
);

CREATE TABLE b (
  id INT
);
`

	stmts := splitStatements(src)
	if len(stmts) != 2 {
		t.Fatalf("应切出 2 条语句,实际 %d 条: %#v", len(stmts), stmts)
	}
	if !strings.Contains(stmts[0], "CREATE TABLE a") {
		t.Errorf("第 1 条不对: %q", stmts[0])
	}
	if !strings.Contains(stmts[1], "CREATE TABLE b") {
		t.Errorf("第 2 条不对: %q", stmts[1])
	}
	// 注释行应被整行丢弃,行内注释留在语句里交给 MySQL 处理
	if strings.Contains(stmts[0], "文件头注释") {
		t.Errorf("独占一行的注释未被跳过: %q", stmts[0])
	}
}

// TestMigrateDirRejectsEmptyDir 保证目录写错时立刻失败。
//
// 目录为空却静默返回会在后续以"表不存在"的形式报错,离真正的原因很远。
func TestMigrateDirRejectsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	// 不放任何 .sql
	err := (&Store{}).MigrateDir(context.Background(), dir)
	if err == nil {
		t.Fatal("空目录应当报错")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("报错文案应指明是哪个目录: %v", err)
	}
}

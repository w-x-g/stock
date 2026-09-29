# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概览

Go 命令行工具,从 BNB Chain(BSC)上发现新发行的 meme 币,按四个硬性条件筛选后把结果写入 MySQL。

四个条件:

1. 持币人数在区间内(默认 100~2000,两端均为严格不等式)
2. 狙击占比低于上限(默认 5%)
3. Top10 中**每个**持币者的持仓不超过上限(默认 3%,已剔除 LP 与销毁地址)
4. 市值低于上限(默认 $50,000)

只有四项全过的代币才写入 `token_screen_metrics`。

**本仓库是从一个更大的采集系统(含 `backfill` / `watcher` / `enrich` 三个命令与 `meme_tokens` 主表)中抽出来的单命令版本。** 大量注释仍在引用那些已经不存在的东西(`meme_tokens`、`insertColumnCount`、`cmd/watcher`、Serper 等),`.gitignore` 里也还留着 `/backfill`、`/watcher`、`/enrich`。读注释遇到这些名字时不要去找对应代码——它们不在本仓库。被移除的建表脚本留在 `scratch/001_init.sql.removed`(仅作参考,不会被 `MigrateDir` 执行)。

## 常用命令

```bash
go build ./...                       # 构建全部包
go vet ./...                         # 静态检查

go test ./...                        # 全部测试(纯单元测试,不连数据库、不发网络请求)
go test ./internal/enrich -run TestEvaluatePass -v    # 跑单个测试(-run 是正则,前缀即可匹配多个)
go test ./internal/store -run TestSplitStatementsOnRealMigrations -v

go run ./cmd/screen -scan-only       # 只扫链做发现,不消耗 GMGN 配额
go run ./cmd/screen -limit 10 -workers 1 -dump-raw ./scratch   # 首次真跑的推荐姿势
go run ./cmd/screen -fixtures ./internal/enrich/testdata        # 离线全链路,不花配额
go run ./cmd/screen -discover trenches                          # 走 GMGN 战壕榜发现

go run ./cmd/sync -mode export -dir ./data     # 把筛选数据导出成文本文件(提交进 git)
go run ./cmd/sync -mode import -dir ./data     # 拉取后把数据合并进本机库
go run ./cmd/sync -mode import -dry-run        # 只报告会怎么处置,不写库
```

没有 Makefile、没有 CI、没有 Dockerfile。构建产物落在仓库根目录(gitignore 里有 `/screen`、`*.exe`)。

`cmd/screen` 每次启动都会对 `migrations/*.sql` 跑一遍 `MigrateDir`,**不需要单独执行迁移**。

### 命令行 flag 约定

零值表示"用配置里的值",因此 flag 只在需要临时覆盖时传。例外是 `-require-migrated`,它用字符串 `true`/`false`/空 来区分"未指定"与"显式设为 false"。

**`-dry-run`、`-source`、`-order` 三个 flag 只声明了、没有任何代码读取**(`cmd/screen/main.go` 的包注释里还写着 `-dry-run` 的用法,已失效)。要么接上,要么删掉,别以为它们在生效。

### 配置

`internal/config` 加载顺序是 **进程环境变量 > 项目根目录 `.env`**(已存在的环境变量不会被文件覆盖)。缺 `GMGN_API_KEY` 时 `config.Load` **不报错**,由 `cmd/screen` 自己检测并退出——因为 `config` 包可能被别的工具复用。

`.env` 里的 `HTTP_PROXY` / `HTTPS_PROXY` 不是可选项:GMGN 与 NodeReal 的域名被 DNS 污染,必须走本地代理。`config.Load` 会把 `.env` 每个键 `Setenv`,Go 的 `http.DefaultTransport` 走 `ProxyFromEnvironment`,两边正好接上。**症状是"查询失败"但程序继续空转几小时**——所有外部请求都超时时先查代理。

## 架构

### 包职责

| 包 | 职责 |
|---|---|
| `cmd/screen` | 筛选入口:flag 解析、并发编排、进度日志、结果汇总。所有"编排"逻辑都在这里 |
| `cmd/sync` | 跨机器同步:把核验结论与筛选结果导出成文本文件,或从文件合并回本机库 |
| `internal/config` | `.env` + 环境变量 → `Config`;启动时严格校验阈值 |
| `internal/chain` | BSC JSON-RPC 客户端(多端点轮询)。扫 `PairCreated` 事件、按时间估算区块号 |
| `internal/enrich` | 两个外部数据源客户端(GMGN、DexScreener)+ **纯函数判定层** |
| `internal/store` | MySQL 访问;迁移执行;候选池与结果表的读写 |
| `internal/model` | 领域结构(`Token`、`ScreenRecord`) |

### 一次 `screen` 的完整链路

```
① 发现  ──┬─ 链上扫 PairCreated → chain.FilterQuoteTokens 按出现频率挑出新币 → chain_candidates
          └─ 或 GMGN 战壕榜(TrenchesAll 按市值区间二分取全)
                    ↓
② 流动性预筛  DexScreener 批量查(免费,不耗配额);查不到交易对或流动性 < $1 → verdict=nolp
                    ↓
③ 第一段漏斗  GMGN token info(权重 1)→ enrich.PreFilter 判掉未毕业/持币数/市值/蜜罐
                    ↓
④ 第二段漏斗  GMGN security + holders(权重 6)→ enrich.Evaluate 四项全判
                    ↓
⑤ 落库     全过 → token_screen_metrics;否则标记 chain_candidates.verdict
```

`chain_candidates` 是**断点续跑**的基础:扫链用 `INSERT IGNORE` 去重,核验只取 `checked_at IS NULL`,中断重跑不会重复消耗配额。

两条发现路径在 `screener.finish()` 汇合——拿到完整快照之后的判定与落库逻辑只有一份。**新增发现来源时接到 `finish` 上,不要复制判定逻辑。**

### 跨机器同步(cmd/sync)

两台不互联的电脑各跑各的库,靠 git 传数据。`cmd/sync` 导出两个文本文件到 `data/`:

| 文件 | 内容 | 作用 |
|---|---|---|
| `verified.tsv` | `地址 <TAB> verdict <TAB> checked_at`,只有 `rejected` / `nodata` | 负结论缓存,避免另一台重复花 GMGN 配额 |
| `results.jsonl` | 每行一个完整的 `ScreenRecord`(35 列 + checked_at) | 筛选通过的快照,交付物本体 |

`passed` **只出现在 `results.jsonl`**。若它在 tsv 里也有一份,就会出现"候选表说通过、结果表却没有明细"的地址,让两边数据自相矛盾。

**不提交数据库文件本身**。`chain_candidates` 每轮都被批量 UPDATE(15 万行),把 SQLite 那样的单文件库放进 git = 每次提交一整份几十 MB 二进制 blob(二进制几乎无法 delta,跑十几轮就上 GB),而且二进制无法 merge,两台同时跑只能二选一。

#### 合并规则:只填补空白,绝不覆盖本机已有结论

| 本机状态 | 处理 |
|---|---|
| 地址不存在 | 补上(连 `checked_at` 一起),下次扫链不重复核验 |
| 有但无结论(`checked_at IS NULL` 或 `verdict='error'`) | 写入导入的结论 |
| 已有明确结论 | **跳过**,保留本机 |
| `results.jsonl` 中本机已有该结果记录 | **跳过**(先写入者优先) |
| `results.jsonl` 中本机候选表已判不合格 | **拦下**,否则候选表与结果表会自相矛盾 |

导入顺序**必须先 tsv 后 jsonl**:结果表要用候选表的结论判断该不该写。

#### 三条容易踩坏的约束

1. **`checked_at` 必须导出并原样写回。** 它不在 35 列里(由 DB 维护),而两张表的常规写入路径分别会用 DB 时间戳 / `NOW()` 覆盖它。不显式写回的话,每次导入都把时间刷成"现在",文件永远收敛不了、git 每次都是全量伪 diff。
2. **`verifiedUpsertSQL` 的判断条件只读 `verdict`。** MySQL 的 `ON DUPLICATE KEY UPDATE` 从左到右求值,后面的赋值能看到前面刚写入的值。只要两个判断条件都碰 `checked_at`,必然有一种行状态会写坏(见 `internal/store/sync.go` 里那段注释的完整推演)。
3. **导出必须按地址排序。** 导出结果要确定,否则每跑一次行序都在变。绝不能依赖 map 迭代顺序。

#### 两台机器的操作约定

```
git pull                                  先拉,再跑
go run ./cmd/screen ...                   正常筛选
go run ./cmd/sync -mode export -dir ./data
git add data/ && git commit && git push
```

**同一时间只在一台机器上跑 `screen`**,并在跑之前先 pull —— 这样 `data/` 不会被两台同时修改,不会冲突。导出内容是确定且排序的,两边最终收敛到同一份文件;`import` 本身是幂等的(自己的导出导回来零改动),这条有测试守着。

### 判定层是纯函数

`enrich.PreFilter` / `enrich.Evaluate` / `enrich.TrenchesPreFilter` / `enrich.LiquidityFilter` 不碰网络也不碰数据库。这是刻意的:接口字段名未实测的前提下,判定逻辑若只能在真跑时验证,每次调参都要花真配额试错。**改动判定规则时必须补 `internal/enrich/*_test.go` 的 golden 用例。**

`PreFilter` 与 `Evaluate` 的语义**不同,不可互相替代**:`PreFilter` 回答"要不要继续花钱"(缺明细也能给出否决),`Evaluate` 回答"最终是否合格"(缺明细时判 `Inconclusive`)。

### 配额与节流

GMGN 配额是本项目最稀缺的资源,相关设计集中在三处:

- **权重记账**:info=1、security=1、holders=5、trenches=2。`intervalFor(weight)` 让低权重端点按比例缩短间隔;汇总时按 `queried + deep*6` 算总权重。
- **全局节流**:`GMGN.reserve()` 用原子 CAS 推后 `nextAllowed`,**不是每个 worker 各睡一会**(那样并发下等于没限流)。`DexScreener` 同款。全仓库约定**只用 `sync/atomic`,零 `sync.Mutex`**——跨协程汇总一律走 channel(见 `liquidityPrefilter`)。
- **限流不重试**:GMGN 在冷却期内重试会延长封禁,所以 429 绝不退避重试,而是由 `noteQuotaPause` 登记一个**全体暂停时刻**(上限 15 分钟),所有 worker 一起等到解除后自动继续。5xx/网络错误才走指数退避。

### 长任务的容错原则

这个项目跑一轮要几小时,任何"遇到错误就中止"的设计都等于永远跑不完。既有代码一贯遵守:

- 单次扫描分片失败 → 跳过并计数,继续跑(重跑自动补齐)
- 单批写库失败 → `execWithRetry` 退避重试 3 次,仍失败就跳过这 500 条
- 限流 → 全体暂停后继续,**不置 `stop`**;只有鉴权失败才停手
- 调用失败 → **不标记候选**,下轮重新拾取;只有明确的"无数据"/"无流动性"才标记
- DexScreener 请求失败 → 整批原样放行,绝不因为第三方抖动误杀候选

对应的另一条原则是**宁漏勿错**:`ScreenResult.Inconclusive`(不知道)必须与 `Passed=false`(明确不合格)严格区分,后者当前者会让整轮静默产出空结果。同理,一条 LP 都没识别出来时默认判 `Inconclusive`(`SCREEN_STRICT_LP=true`)。

### 数据

`migrations/MigrateDir` 按文件名字典序执行、无版本追踪表、语句全部幂等。

- **已发布的 `.sql` 不可修改**——改了不会被重放,只能新增 `003_`、`004_`
- 新文件必须继续用零填充前缀(字符串比较下 `"010" < "002"`)
- `store.splitStatements` 有两条格式硬约束:语句结尾的 `;` **必须独占行尾**(写成 `x; -- 注释` 会把两条语句粘起来),注释必须**独占一行**。`store/migrate_test.go` 会对真实迁移文件断言这两点

两张表:`chain_candidates`(候选池 + `verdict` 状态机)、`token_screen_metrics`(只存通过者,`uk_contract` 保证每合约一条,冲突时全字段覆盖)。

`store/screen.go` 的 `screenColumns` / `screenColumnCount` / `screenArgs` / upsert 子句**四者必须同步修改**,`TestScreenColumnArity` 会守住这一点。

## 核验结论的标记规则

链上路径(`runChain` → `verifyOne`)对每个候选**只判定一次**(`screener.finish` 返回结论与"是否已落库"两样东西),然后分三类处置。这张表是不变式,改动 `verifyOne` 前先读它:

| 结论 | 候选标记 | 为什么 |
|---|---|---|
| 通过 **且结果已落库** | `passed` | 落库失败就**不标记** —— 标记了就不再重试,而结果没进库,这条通过记录会永久丢失 |
| 明确不合格 | `rejected` | |
| `Inconclusive`(数据不足) | **不标记** | 留 `checked_at` 为 NULL,下轮自动重核验 |

第三条是刻意的:`Inconclusive` 不等于"不合格"。给它写 `checked_at` 等于把一次数据缺失永久固化成否决结论,而 `internal/enrich/screen.go` 明确要求这两者必须区分("把前者当后者会让整轮筛选静默产出空结果,是最危险的失败模式")。

代价要清楚:**永远判不出结果的币会每轮重花一次配额**(比如怎么都识别不出 LP 的币,每轮都要重取权重 6 的持币明细)。`report()` 会把"无法确定"和"失败未落库"的数量单独打出来提醒。真到了这类币多到值得处理的时候,再加一个尝试计数字段把它们挡掉——那需要新迁移。

## 约定

- 所有率值在**解析层**统一归一化成百分数 0~100(接口原始值多为 0~1 小数)。下游代码不再做量纲判断。
- 日志使用 emoji 前缀区分语义(`⛓ 扫链`、`💧 预筛`、`⊘ 淘汰`、`✅ 通过`、`⏸ 限流`、`⚠️ 警告`)。粗筛淘汰原因只打印前 12 条(`prefilterLogLimit`)——几万个候选全打会淹没真正通过的那几条。
- 空字符串与 NULL 语义不同(前者"确实取到空值",后者"没有这个信息"),写库前用 `nullString` / `nullableJSON` / `nullIfEmpty` 转换。
- 迁移脚本兼容 **MySQL 5.7**(不用 CTE、不用窗口函数)。

## 已知的注释与代码不符

- **`launchpad_status` 的"已毕业"取值是 `1`,不是 2。** 权威定义在 `internal/enrich/gmgn.go` 的 `gmgnLaunchpadLive = 1`(附实测证据:15.8 万个候选里没有一个 status 是 2)。但 `internal/enrich/screen.go` 的 `RequireMigrated` 注释与 `migrations/001_screening.sql` 的表注释仍写着 `==2`,是过期的。
- `cmd/screen` 包注释里的 `-dry-run` 用法未实现(见上文 flag 一节),且其中"候选来自本库已有的 meme_tokens 表"描述的是重构前的架构。
- **`token_screen_metrics.checked_at` 不会自动更新。** `internal/store/screen.go` 的 upsert 注释写着"checked_at 由 DB 自动更新",但 `migrations/001_screening.sql` 里它是 `TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP`、**没有 `ON UPDATE`**,也不在 `screenUpsertClause` 里。所以它的实际语义是"首次写入时刻",不是"最后一次判定时刻"。这直接影响任何"比时间戳判新旧"的逻辑。
- `migrations/002_candidates.sql` 的 `verdict` 注释只列了 4 种取值,**漏了 `nolp`**(`internal/store/candidates.go` 里实际有 5 种),而且没提 `DEFAULT NULL` 表示"尚未核验"。
- `internal/enrich/testdata/*.json` 的字段名取自官方 SKILL.md,**不是实测样本**。在做过一次 `-dump-raw` 并把真实响应换进去之前,测试通过只说明判定逻辑自洽,不说明线上字段名取对了。GMGN 的字段名在 snake_case / camelCase 之间摇摆,解析层因此用表驱动的 `gmgnFields.pick(...)` 接受多个候选键名——**校准时改这里,不要在别处加 if 分叉**。

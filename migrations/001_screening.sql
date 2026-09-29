-- GMGN 筛选结果表 —— 条件筛选通过的 meme 币
-- 目标库: stock
-- 兼容性: MySQL 5.7(不使用 CTE / 窗口函数,均为 8.0 特性)
--
-- 本项目的迁移按文件名字典序执行,语句全部幂等(IF NOT EXISTS),
-- 因此重复执行等价于"已是最新"。代价是**已发布的 .sql 不可再修改**——
-- 改动不会被重放,必须新增一个文件(003_、004_……)。
--
-- 两条格式硬约束来自 store.splitStatements 的切分规则:
--   1. 语句结尾的 ';' 必须独占行尾,不能写成 "x; -- 注释",
--      否则切分点识别不到,会把后一条语句粘进来
--   2. 注释必须独占一行(以 -- 开头的行会被整行跳过)

-- ---------------------------------------------------------------------------
-- 筛选结果:只存四个条件全部通过的代币,每个合约地址保留最新一条
--
-- 刻意不给 meme_tokens 加列:
--   * 主表是 backfill/watcher 的热路径,列数被 insertColumnCount 硬编码约束
--   * 这里的指标是分钟级变化的快照,主表一列只能存"最后一次"
--   * 主表是百万行级,需要这些指标的只有极小一撮,稀疏列会摊高全表扫描成本
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS token_screen_metrics (
  id                 BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  contract_address   VARCHAR(42) NOT NULL,
  name               VARCHAR(255)  DEFAULT NULL,
  symbol             VARCHAR(100)  DEFAULT NULL,
  source             ENUM('four_meme','pancake_v2') DEFAULT NULL,
  launched_at        DATETIME      NOT NULL,

  -- ---- 原始指标,全部取自同一次 GMGN 快照 ----
  holder_count       INT UNSIGNED  DEFAULT NULL,
  sniper_count       INT UNSIGNED  DEFAULT NULL,
  market_cap         DECIMAL(20,2) DEFAULT NULL,
  liquidity          DECIMAL(20,2) DEFAULT NULL,

  -- ---- 发行状态 ----
  -- launchpad_status: 0=未开盘, 1=进行中, 2=已迁移到 DEX(即"已毕业")
  -- 这是比链上 graduated 标志更权威的判据 —— 后者对 four.meme 几乎从不置位
  launchpad_status    TINYINT      DEFAULT NULL,
  launchpad_progress  DECIMAL(9,4) DEFAULT NULL,
  migration_market_cap DECIMAL(20,2) DEFAULT NULL,
  creator_address     VARCHAR(42)  DEFAULT NULL,
  -- creator_token_status: hold=仍在持有, sell=已清仓
  creator_token_status VARCHAR(16) DEFAULT NULL,

  -- 比率一律归一化为百分数 0~100。接口原始值多为 0~1 小数,
  -- 量纲换算集中在解析层一处完成,下游不必反复判断"这个 0.05 是 5% 还是 0.05%"
  sniper_rate        DECIMAL(9,4)  DEFAULT NULL,
  sniper_rate_basis  VARCHAR(8)    DEFAULT NULL,
  sniper_count_rate  DECIMAL(9,4)  DEFAULT NULL,
  -- 接口直接给出的狙击持仓占比(stat.top70_sniper_hold_rate),不必从明细凑
  sniper_hold_rate   DECIMAL(9,4)  DEFAULT NULL,
  bundler_rate       DECIMAL(9,4)  DEFAULT NULL,
  top10_rate         DECIMAL(9,4)  DEFAULT NULL,
  top10_rate_no_lp   DECIMAL(9,4)  DEFAULT NULL,
  top10_max_rate     DECIMAL(9,4)  DEFAULT NULL,
  top10_count        INT UNSIGNED  DEFAULT NULL,
  lp_excluded_count  SMALLINT UNSIGNED DEFAULT NULL,
  lp_excluded_rate   DECIMAL(9,4)  DEFAULT NULL,
  -- 命中多条依据时会拼起来(如 addr_type_burn+addr_type_pool = 29 字符),
  -- 16 位不够用——这是实测撞出来的
  lp_basis           VARCHAR(64)   DEFAULT NULL,
  holder_coverage    DECIMAL(9,4)  DEFAULT NULL,
  creator_rate       DECIMAL(9,4)  DEFAULT NULL,

  -- ---- 安全检测(token security 端点) ----
  -- 蜜罐仅 BSC/Base 可测:买得进卖不出,是硬性否决项
  is_honeypot        TINYINT(1)    DEFAULT NULL,
  buy_tax            DECIMAL(9,4)  DEFAULT NULL,
  sell_tax           DECIMAL(9,4)  DEFAULT NULL,
  is_open_source     TINYINT(1)    DEFAULT NULL,
  is_renounced       TINYINT(1)    DEFAULT NULL,

  -- 本次判定所用的阈值快照。日后回溯"当时是按什么标准筛出来的"要靠它。
  criteria_json      TEXT,

  -- 剔除前的持币明细。存剔除前而非剔除后:日后调整 LP 剔除规则可以离线重算,
  -- 不必再花一次 GMGN 配额
  holders_json       JSON,

  checked_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  -- 每个合约地址只保留最新一条:重跑时新数据覆盖旧数据
  UNIQUE KEY uk_contract (contract_address),
  KEY idx_mcap (market_cap),
  KEY idx_checked (checked_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='GMGN 筛选通过结果';

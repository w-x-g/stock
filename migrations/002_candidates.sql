-- 链上发现的候选代币 —— 等待 GMGN 核验
-- 目标库: stock
-- 兼容性: MySQL 5.7(不使用 CTE / 窗口函数)
--
-- 为什么需要这张表:GMGN 的"战壕"列表是精选榜,会漏掉已毕业的币(实测)。
-- 唯一完整的来源是链上的 PancakeSwap PairCreated 事件。这张表把链上扫到的
-- 地址沉淀下来,并记录哪些还没问过 GMGN。
--
-- 它同时是**断点续跑**的基础:扫链阶段用 INSERT IGNORE 幂等去重,
-- 核验阶段只取 checked_at IS NULL 的,中断后重跑不会重复消耗 GMGN 配额。
--
-- 格式硬约束(见 store.splitStatements):语句结尾的 ';' 必须独占行尾,
-- 注释必须独占一行。

CREATE TABLE IF NOT EXISTS chain_candidates (
  contract_address VARCHAR(42) NOT NULL,
  -- 交易对合约地址与建池位置,用于排障
  pair_address     VARCHAR(42)     DEFAULT NULL,
  pool_block       BIGINT UNSIGNED DEFAULT NULL,
  pool_tx          VARCHAR(66)     DEFAULT NULL,
  discovered_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  -- NULL 表示还没问过 GMGN;非 NULL 表示已核验过
  checked_at       TIMESTAMP NULL DEFAULT NULL,
  -- passed=通过全部条件并已入 token_screen_metrics
  -- rejected=被条件筛掉
  -- nodata=GMGN 没有这个币的数据
  -- error=调用失败(可重试)
  verdict          VARCHAR(16)     DEFAULT NULL,
  UNIQUE KEY uk_contract (contract_address),
  KEY idx_pending (checked_at),
  KEY idx_verdict (verdict)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='链上发现的建池代币(待 GMGN 核验)';

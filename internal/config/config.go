// Package config 负责加载与校验运行配置。
//
// 配置来源优先级:进程环境变量 > 项目根目录 .env 文件。
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config 是全部运行配置。
type Config struct {
	MySQL       MySQL
	GMGN        GMGN
	DexScreener DexScreener
	Chain       Chain
	Screen      Screen
}

// Chain 是链上读取(JSON-RPC)的配置。
//
// 它服务于"发现"这一步:GMGN 的战壕列表会漏币,唯一完整的来源是链上的
// PancakeSwap PairCreated 事件。
type Chain struct {
	// RPCEndpoints 是 BSC 的 HTTP JSON-RPC 端点。多端点可分摊限流——
	// 扫 5 天要发上百次 getLogs,单端点很容易被打满。
	RPCEndpoints []string
	// ChunkSize 是单次 eth_getLogs 覆盖的区块数。
	// 节点普遍有上限(且随负载浮动),所以代码在超限时会自动对半拆分,
	// 这个值只是起点,取大一点能少发几次请求。
	ChunkSize uint64
	// QuoteThreshold 是"出现多少次算计价币"的阈值。
	// 计价币会出现在成百上千个池子里,普通 meme 币最多建几个池。
	QuoteThreshold int
}

// MySQL 是数据库连接配置。
type MySQL struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

// DSN 返回 go-sql-driver/mysql 使用的连接串。
//
// parseTime=true 让 DATETIME 直接扫描进 time.Time;loc=Local 保证时区一致。
func (m MySQL) DSN() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=Local"+
			"&timeout=10s&readTimeout=30s&writeTimeout=30s",
		m.User, m.Password, m.Host, m.Port, m.Database)
}

// GMGN 是链上指标服务(GMGN Agent API)的配置。
type GMGN struct {
	BaseURL string
	// APIKey 为空时 cmd/screen 会直接报错退出——但**不在这里校验**,
	// 这样别的工具复用本包时不会因为缺 key 就起不来。
	APIKey string
	// Chain 是请求路径里的链名。接口要的是 "bsc" 这类短名,不要与 ChainID(56) 混淆。
	Chain string
	// HolderLimit 是单次请求返回的持币者条数。条件 3 名义上只需前 10 名,
	// 但覆盖度越高,"狙击占比"的下界越紧,故默认取 100。
	HolderLimit int
	// MinIntervalMS 是最小请求间隔(毫秒),以**权重 5 的订单薄端点**为基准。
	// 权重更低的端点按比例缩短间隔——见 enrich.GMGN.intervalFor。
	MinIntervalMS int
}

// DexScreener 是免费行情接口的配置。
//
// 它服务于"流动性预筛":在消耗 GMGN 配额之前,先用它把链上建了空池的币淘汰掉。
// 实测这一步能拦下八成以上候选且不误杀已毕业的币——推导见 enrich/dexscreener.go。
type DexScreener struct {
	BaseURL string
	// Batch 是单次批量查询的地址数。上限 30 是接口的硬限制,超过会被收敛。
	Batch int
	// MinLiquidityUSD 是流动性门槛(美元),低于它的候选直接淘汰。
	// 默认 1:样本里已毕业币的最低流动性是 3.48,留了余量。
	MinLiquidityUSD float64
	// MinIntervalMS 是最小请求间隔(毫秒)。该接口免费且宽松,可远小于 GMGN。
	MinIntervalMS int
	// Enabled 决定是否启用预筛。关掉即回到"每个候选都问 GMGN"的老路——
	// 结果一样,只是慢数倍,留这个开关是为了在接口不可用时能退回去。
	Enabled bool
}

// Screen 是 meme 币筛选的阈值配置。
//
// 全部可由 cmd/screen 的命令行 flag 覆盖(沿用"零值表示用配置"的约定)。
type Screen struct {
	// Days 是候选的时间窗口(天)。
	Days int
	// MinHolders / MaxHolders 是持币人数区间:**下界含等于(≥),上界不含(<)**。
	//
	// 两端语义不对称是刻意的——需求里下界写作"不少于 N 人",上界写作"低于 N 人"。
	// 判定的另一半在 enrich.ScreenCriteria。
	MinHolders int64
	MaxHolders int64
	// MaxSniperRate 是狙击占比上限(百分数,严格小于)。
	MaxSniperRate float64
	// SniperBasis 决定狙击占比怎么算:"count"(sniper_wallets/holder_count)或
	// "amount"(top70_sniper_hold_rate,即狙击持仓占比)。
	SniperBasis string
	// MaxTop10Rate 是 Top10 中**单个**持币者的持仓上限(百分数)。
	MaxTop10Rate float64
	// TopN 是参与上述判断的持币者个数,默认 10。
	TopN int
	// MaxMarketCap 是市值上限(美元,严格小于)。
	MaxMarketCap float64
	// RequireMigrated 要求代币已完成联合曲线、迁移到公开市场(即"已毕业")。
	RequireMigrated bool
	// StrictLP 决定"一条 LP 都没识别出来"时如何处理:
	// true=判为无法确定(淘汰),false=按原始明细照判。默认 true。
	StrictLP bool
}

// 筛选条件的默认阈值,与用户需求一一对应。
const (
	DefaultScreenDays          = 5
	DefaultScreenMinHolders    = 300
	DefaultScreenMaxHolders    = 2000
	DefaultScreenMaxSniperRate = 5.0
	DefaultScreenMaxTop10Rate  = 3.0
	DefaultScreenTopN          = 10
	DefaultScreenMaxMarketCap  = 1_000_000.0
)

// GMGN 的默认配置。
const (
	// 是 openapi 不是 api——后者是不存在的域名,照它写会一直连不上。
	DefaultGMGNBaseURL       = "https://openapi.gmgn.ai"
	DefaultGMGNChain         = "bsc"
	DefaultGMGNHolderLimit   = 100
	DefaultGMGNMinIntervalMS = 1200
)

// DexScreener 的默认配置。
const (
	DefaultDexScreenerBaseURL = "https://api.dexscreener.com"
	// 30 是接口的硬上限,再高会被拒。
	DefaultDexScreenerBatch = 30
	// 1 美元:已毕业币样本里的最低流动性是 3.48,这个门槛只拦空池,不误杀。
	DefaultDexScreenerMinLiquidity = 1.0
	// 250ms(约 240 次/分钟)对应官方给的 300 次/分钟额度,留了一点余量。
	DefaultDexScreenerMinIntervalMS = 250
)

// Load 从 .env 与环境变量加载配置。
//
// root 为项目根目录,.env 应位于其中。若 .env 不存在,则完全依赖环境变量,
// 这不视为错误——生产环境通常直接注入环境变量。
func Load(root string) (*Config, error) {
	envPath := filepath.Join(root, ".env")
	if err := loadDotEnv(envPath); err != nil {
		return nil, fmt.Errorf("加载 %s 失败: %w", envPath, err)
	}

	cfg := &Config{
		MySQL: MySQL{
			Host:     envOr("MYSQL_HOST", "127.0.0.1"),
			Port:     envIntOr("MYSQL_PORT", 3306),
			User:     envOr("MYSQL_USER", "root"),
			Password: os.Getenv("MYSQL_PASSWORD"),
			Database: envOr("MYSQL_DATABASE", "stock"),
		},
		Chain: Chain{
			RPCEndpoints:   splitList(os.Getenv("RPC_ENDPOINTS")),
			ChunkSize:      envUint64Or("CHAIN_CHUNK_SIZE", 50_000),
			QuoteThreshold: envIntOr("CHAIN_QUOTE_THRESHOLD", 50),
		},
		GMGN: GMGN{
			BaseURL:       envOr("GMGN_BASE_URL", DefaultGMGNBaseURL),
			APIKey:        os.Getenv("GMGN_API_KEY"),
			Chain:         envOr("GMGN_CHAIN", DefaultGMGNChain),
			HolderLimit:   envIntOr("GMGN_HOLDER_LIMIT", DefaultGMGNHolderLimit),
			MinIntervalMS: envIntOr("GMGN_MIN_INTERVAL_MS", DefaultGMGNMinIntervalMS),
		},
		DexScreener: DexScreener{
			BaseURL:         envOr("DEXSCREENER_BASE_URL", DefaultDexScreenerBaseURL),
			Batch:           envIntOr("DEXSCREENER_BATCH", DefaultDexScreenerBatch),
			MinLiquidityUSD: envFloatOr("DEXSCREENER_MIN_LIQUIDITY", DefaultDexScreenerMinLiquidity),
			MinIntervalMS:   envIntOr("DEXSCREENER_MIN_INTERVAL_MS", DefaultDexScreenerMinIntervalMS),
			Enabled:         envBoolOr("DEXSCREENER_ENABLED", true),
		},
		Screen: Screen{
			Days:            envIntOr("SCREEN_DAYS", DefaultScreenDays),
			MinHolders:      int64(envIntOr("SCREEN_MIN_HOLDERS", DefaultScreenMinHolders)),
			MaxHolders:      int64(envIntOr("SCREEN_MAX_HOLDERS", DefaultScreenMaxHolders)),
			MaxSniperRate:   envFloatOr("SCREEN_MAX_SNIPER_RATE", DefaultScreenMaxSniperRate),
			SniperBasis:     envOr("SCREEN_SNIPER_BASIS", "count"),
			MaxTop10Rate:    envFloatOr("SCREEN_MAX_TOP10_RATE", DefaultScreenMaxTop10Rate),
			TopN:            envIntOr("SCREEN_TOP_N", DefaultScreenTopN),
			MaxMarketCap:    envFloatOr("SCREEN_MAX_MARKET_CAP", DefaultScreenMaxMarketCap),
			RequireMigrated: envBoolOr("SCREEN_REQUIRE_MIGRATED", true),
			StrictLP:        envBoolOr("SCREEN_STRICT_LP", true),
		},
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate 确保必需项存在,且参数在安全范围内。
//
// 这里刻意做得严格:配置错误在启动时立刻暴露,好过跑到一半才失败。
//
// 刻意**不校验 GMGN.APIKey**:缺 key 由 cmd/screen 自己报错并给出申请指引,
// 而本包可能被别的工具复用,不该因此启动不了。
func (c *Config) validate() error {
	if c.MySQL.Database == "" {
		return fmt.Errorf("MYSQL_DATABASE 未配置")
	}
	return c.validateScreen()
}

// validateScreen 校验筛选阈值。
//
// 全部在启动时拦截:阈值自相矛盾时,跑了几百次 API 调用才发现是纯粹的浪费——
// GMGN 配额是本项目最稀缺的资源。
func (c *Config) validateScreen() error {
	s := c.Screen

	if s.Days < 1 {
		return fmt.Errorf("SCREEN_DAYS=%d 非法:至少为 1", s.Days)
	}
	if s.MinHolders >= s.MaxHolders {
		return fmt.Errorf("持币人数区间非法:下界 %d 必须小于上界 %d", s.MinHolders, s.MaxHolders)
	}
	if s.MaxSniperRate <= 0 || s.MaxSniperRate > 100 {
		return fmt.Errorf("SCREEN_MAX_SNIPER_RATE=%v 非法:必须在 (0,100] 之间", s.MaxSniperRate)
	}
	if s.SniperBasis != "count" && s.SniperBasis != "amount" {
		return fmt.Errorf("SCREEN_SNIPER_BASIS=%q 非法:必须是 count 或 amount", s.SniperBasis)
	}
	if s.MaxTop10Rate <= 0 || s.MaxTop10Rate > 100 {
		return fmt.Errorf("SCREEN_MAX_TOP10_RATE=%v 非法:必须在 (0,100] 之间", s.MaxTop10Rate)
	}
	if s.TopN < 1 {
		return fmt.Errorf("SCREEN_TOP_N=%d 非法:至少为 1", s.TopN)
	}
	if s.MaxMarketCap <= 0 {
		return fmt.Errorf("SCREEN_MAX_MARKET_CAP=%v 非法:必须为正数", s.MaxMarketCap)
	}
	// Top10 判定至少要取满 TopN 条,再留一条用于识别 LP 的余量。
	// 取不满时判定会退化成"按拿到的几条算",结论没有意义。
	if c.GMGN.HolderLimit < s.TopN+1 {
		return fmt.Errorf("GMGN_HOLDER_LIMIT=%d 非法:至少要大于 SCREEN_TOP_N(%d),否则取不满待判定的持币者",
			c.GMGN.HolderLimit, s.TopN)
	}
	if c.GMGN.MinIntervalMS < 0 {
		return fmt.Errorf("GMGN_MIN_INTERVAL_MS=%d 非法:不能为负", c.GMGN.MinIntervalMS)
	}
	return nil
}

// loadDotEnv 解析 .env 文件并注入进程环境。
//
// 已存在的环境变量优先,不会被文件覆盖——这样命令行可临时覆盖配置。
// 文件不存在时静默返回 nil。
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)

		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, val); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envUint64Or(key string, def uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// splitList 把逗号分隔的字符串切成列表,并剔除空项。
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envBoolOr(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

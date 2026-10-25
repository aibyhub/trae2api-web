// constants.go SOLO 上游技术常量（SPEC §1，来自实测，禁止改动）。
package upstream

const (
	AgentHost      = "https://trae-api-cn.mchost.guru"
	UgHost         = "https://api.trae.cn"
	OAuthHost      = "https://api.trae.com.cn"
	ConsoleHost    = "https://www.trae.cn"
	ClientID       = "en1oxy7wnw8j9n" // SOLO stable
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.64" // 真机 aha manifest appVersion（UA/X-Ide-Version/登录页对齐真机）
	IdeVersionCode = "20260811"
	// TronBuildVersion 真机 manifest.json buildVersion（登录页 plugin_version 取
	// tronBuildVersion，见真机 out/main.js buildLoginUrl：plugin_version=${this.a.tronBuildVersion}）。
	// 2026-10 客户端 1.107.1 实测值；此前网关用旧值 2.3.62834 与真机指纹不符。
	TronBuildVersion = "2.3.82597"
	DeviceBrand      = "83DG"
	// DeviceCPU 真机原生日志 add_header 实录：x-device-cpu 为实际 CPU 型号。
	// 网关单机部署即单一设备身份，取本机实值对齐（2026-10-25 抓包）。
	DeviceCPU    = "Intel(R) Core(TM) i7-8750H CPU"
	OSVersion    = "Windows 11 Pro"
	DeviceOSName = "windows" // commonParams.os_name（fb() → X-Device-Type）
	AppVersion   = "0.1.64"  // 真机 aha manifest appVersion（UG 通道 X-App-Version / UG UA）
	Function     = "solo_work_lite"

	// 端点
	EpChat   = "/api/agent/v3/llm_utils_chat"
	EpModels = "/api/ide/v1/get_detail_param"
	// EpModelsBatch 新客户端主力菜单端点（真机 2026-06 会话日志 155 次 vs 旧端点 0 次）。
	// 请求/响应 schema 未逐字段验证：FetchModels 先试 batch，任何失败回退 EpModels。
	EpModelsBatch = "/api/ide/v1/batch_get_detail_param"
	// EpExchangeV3 2026-10 客户端 1.107.1 迁移后的新 OAuth 端点（out/main.js 实测）：
	// 请求体 {ClientID, ClientSecret:"", RefreshToken, DeviceInfo, DeviceProof, IDEVersion}，
	// DeviceProof = EC P-256 ECDSA-SHA256 签名（见 deviceproof.go）。旧端点 EpExchange
	// 当前仍工作，v3 由 TW2A_EXCHANGE_V3 开关控制（默认关，待真机联调后切）。
	EpExchangeV3    = "/trae/api/v3/oauth/ExchangeToken"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)

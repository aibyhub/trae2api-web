// headers.go SOLO 三类请求头：对话（SOLOHeaders）/ ug（UgHeaders）/ oauth（OAuthHeaders）。
package upstream

import (
	"net/http"

	"trae2api-web/internal/auth"
)

const clientUA = "Trae/" + IdeVersion

// ugUserAgent UG 通道 UA。真实客户端 UG 请求走 Electron net.fetch（iCubeBaseTTNetService），
// UA 是 Chromium 形态（Electron 39.2.7 / Chromium 142，aha manifest appVersion=0.1.64），
// 而非 SOLO 通道的 Trae/x 短 UA——旧实现发 Trae/0.1.52 与真实客户端指纹不符。
const ugUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) TRAE SOLO CN/" + AppVersion + " Chrome/142.0.0.0 Electron/39.2.7 Safari/537.36"

// SOLOHeaders 设置 llm_utils_chat / get_detail_param 所需的 SOLO 专属头。
// 规则来自 SPEC §1 SOLO headers（实测必须）。
func SOLOHeaders(req *http.Request, a *auth.Auth, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", clientUA)
	at := a.JWT() // 读锁快照，防与 RefreshToken 写并发竞态
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", IdeVersion)
	req.Header.Set("X-Ide-Version-Code", IdeVersionCode)
	req.Header.Set("X-App-Version-Code", IdeVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// UgHeaders 设置签到/积分（api.trae.cn）所需头。
// 协议逆向自 TRAE SOLO CN 0.1.64 主进程（resources/app/out/main.js）：
// UG 请求的应用层头 = cb()（Content-Type + Authorization）+ fb()（5 个设备头），
// 共 7 个，经 Electron net.fetch 原样透传。真实客户端不发 X-Machine-Id /
// X-User-Region / X-Cloudide-Token，这里不再多发（v1.1.3 及之前多发头属指纹偏离）。
// X-Device-Id 取自 auth 文件（登录时随 OAuth 提交给上游的设备对）；
// 导入路径随机生成的设备对未在上游注册，claim 可能仍被 9074 拒绝——
// 届时用面板重登，或导入时以真实客户端设备号覆盖（见 server.accounts.go）。
func UgHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT()) // 读锁快照
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
	req.Header.Set("X-Device-Brand", DeviceBrand) // commonParams.device_model
	req.Header.Set("X-Device-Type", DeviceOSName) // commonParams.os_name
	req.Header.Set("X-OS-Version", OSVersion)     // commonParams.os_version
	req.Header.Set("X-App-Version", AppVersion)   // commonParams.app_version
	req.Header.Set("User-Agent", ugUserAgent)
}

// OAuthHeaders 设置 ExchangeToken / GetUserInfo 所需头（无签名，仅 UA）。
func OAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
}

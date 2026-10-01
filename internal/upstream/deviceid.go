// deviceid.go UG 通道设备号（x-device-id）的生成与校验。
//
// 2026-10-01 实测（与 52pojie 社区帖互证，见 RESEARCH）：
//   - claim 的设备校验只看格式：必须是十进制数字（真实客户端 aha 设备号 15~16 位；
//     社区工具放宽 12~20 位）。UUID / 32 位 hex 直接 9074「当前参与用户太多」
//     （误导性反滥用文案，与 IP / 请求头无关）。
//   - 生成的未注册十进制号可用，无需真实 device_register。
//   - 设备配额「每设备每日一次、跨账号共享」→ 每账号必须持有独立设备号，
//     否则同设备第二个账号 claim 会被 9095「当前设备今日已经签到」挡住。
package upstream

import (
	"crypto/rand"
	"math/big"
	"regexp"
)

// decimalDeviceIDRe 十进制设备号：12~20 位（真实客户端 15~16 位）。
// 迁移/校验按此认定有效，已有效的号不做无谓更换。
var decimalDeviceIDRe = regexp.MustCompile(`^[0-9]{12,20}$`)

// IsDecimalDeviceID 报告 s 是否为上游接受的十进制设备号形态。
func IsDecimalDeviceID(s string) bool { return decimalDeviceIDRe.MatchString(s) }

// NewDeviceID 生成 16 位十进制设备号（首位 2-7，与实测可用的号段一致；
// 真实客户端形态即 16 位十进制）。crypto/rand 驱动。
func NewDeviceID() (string, error) {
	first, err := rand.Int(rand.Reader, big.NewInt(6))
	if err != nil {
		return "", err
	}
	digits := make([]byte, 16)
	digits[0] = '2' + byte(first.Int64())
	for i := 1; i < 16; i++ {
		d, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		digits[i] = '0' + byte(d.Int64())
	}
	return string(digits), nil
}

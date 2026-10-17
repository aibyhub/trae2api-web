// transient_test.go 签到瞬时故障判定的回归边界：
// 超时/握手/连接类 → 可重试；上游业务拒绝（9074 设备未注册等）→ 不重试。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestTransientNetErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"业务9074", fmt.Errorf("checkin claim: code=9074 当前参与用户太多"), false},
		{"业务9095", fmt.Errorf("checkin claim: code=9095 当前设备今日已经签到"), false},
		{"解析失败", fmt.Errorf("checkin status parse: invalid character 'x'"), false},
		{"头等待超时（历史152例）", &url.Error{Op: "Post", URL: "https://api.trae.cn/x", Err: errors.New("net/http: timeout awaiting response headers")}, true},
		{"tlsfp握手超时", fmt.Errorf("tlsfp: handshake api.trae.cn:443: context deadline exceeded"), true},
		{"context deadline", fmt.Errorf("Post \"https://x\": %w", context.DeadlineExceeded), true},
		{"连接拒绝", errors.New("proxyconnect tcp: dial tcp 1.2.3.4:1080: connect: connection refused"), true},
		{"连接重置", errors.New("read tcp 1.2.3.4:5000->5.6.7.8:443: connection reset by peer"), true},
		{"EOF", errors.New("Post https://x: : EOF"), true},
		{"包裹的业务错", fmt.Errorf("checkin status: %w", errors.New("code=9074 设备未注册")), false},
	}
	for _, c := range cases {
		if got := transientNetErr(c.err); got != c.want {
			t.Errorf("%s: transientNetErr=%v want %v (err=%v)", c.name, got, c.want, c.err)
		}
	}
}

package aliyun

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAliyun 是一个会验签的假阿里云:签名对不上就回 SignatureDoesNotMatch。
//
// 验签用独立的一份实现(不调 sign)—— 与生产代码共用一份的话,
// 编码规则写错时两边一起错,测试永远是绿的。
type fakeAliyun struct {
	t       *testing.T
	secret  string
	handler func(action string, form map[string]string, w http.ResponseWriter)
	calls   atomic.Int32
}

func (f *fakeAliyun) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if err := r.ParseForm(); err != nil {
		f.t.Fatalf("解析表单: %v", err)
	}
	form := map[string]string{}
	for k := range r.PostForm {
		form[k] = r.PostForm.Get(k)
	}
	got := form["Signature"]
	keys := make([]string, 0, len(form))
	for k := range form {
		if k != "Signature" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, percentEncode(k)+"="+percentEncode(form[k]))
	}
	toSign := "POST&%2F&" + percentEncode(strings.Join(parts, "&"))
	mac := hmac.New(sha1.New, []byte(f.secret+"&"))
	mac.Write([]byte(toSign))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got != want {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"Code":"SignatureDoesNotMatch","Message":"bad signature","RequestId":"r1"}`))
		return
	}
	f.handler(form["Action"], form, w)
}

func newTestClient(t *testing.T, f *fakeAliyun) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New(
		WithEndpoint(func(string) string { return srv.URL + "/" }),
		WithClock(func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }),
		WithNoSleep(),
	)
	return c, srv
}

var testCreds = Credentials{AccessKeyID: "LTAI5tTESTKEYID1234", AccessKeySecret: "s3cr3t+with/special*chars~"}

func TestSignatureVerifiesAgainstIndependentImplementation(t *testing.T) {
	f := &fakeAliyun{t: t, secret: testCreds.AccessKeySecret}
	f.handler = func(action string, form map[string]string, w http.ResponseWriter) {
		// 带上会触发编码规则的参数值:空格、星号、波浪线、加号。
		if form["Version"] != cdtVersion || form["RegionId"] != cdtRegion {
			t.Errorf("CDT 请求的版本 / 区域不对: %v", form)
		}
		_, _ = w.Write([]byte(`{"RequestId":"x","TrafficDetails":[{"BusinessRegionId":"cn-hongkong","Traffic":1024}]}`))
	}
	c, _ := newTestClient(t, f)
	got, err := c.ListCdtInternetTraffic(context.Background(), testCreds)
	if err != nil {
		t.Fatalf("签名校验没通过: %v", err)
	}
	if len(got) != 1 || got[0].Bytes != 1024 {
		t.Fatalf("解析结果 = %+v", got)
	}
}

func TestPercentEncodeFollowsAliyunRules(t *testing.T) {
	cases := map[string]string{
		"a b":     "a%20b",
		"a*b":     "a%2Ab",
		"a~b":     "a~b",
		"a+b":     "a%2Bb",
		"2026-09": "2026-09",
		"x/y":     "x%2Fy",
	}
	for in, want := range cases {
		if got := percentEncode(in); got != want {
			t.Errorf("percentEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCdtTrafficAcceptsBothShapes(t *testing.T) {
	shapes := map[string]string{
		"顶层":        `{"TrafficDetails":[{"BusinessRegionId":"cn-hongkong","Traffic":"100"},{"BusinessRegionId":"cn-hangzhou","Traffic":7}]}`,
		"套在 Data 下": `{"Code":"200","Data":{"TrafficDetails":[{"BusinessRegionId":"cn-hongkong","Traffic":100},{"BusinessRegionId":"cn-hangzhou","Traffic":7}]}}`,
	}
	for name, body := range shapes {
		t.Run(name, func(t *testing.T) {
			f := &fakeAliyun{t: t, secret: testCreds.AccessKeySecret}
			f.handler = func(_ string, _ map[string]string, w http.ResponseWriter) { _, _ = w.Write([]byte(body)) }
			c, _ := newTestClient(t, f)
			got, err := c.ListCdtInternetTraffic(context.Background(), testCreds)
			if err != nil {
				t.Fatal(err)
			}
			sums := SumByClass(got)
			if sums[ClassInternational] != 100 || sums[ClassChina] != 7 {
				t.Fatalf("按类求和 = %v", sums)
			}
		})
	}
}

func TestCdtMissingTrafficDetailsIsAnErrorNotZero(t *testing.T) {
	f := &fakeAliyun{t: t, secret: testCreds.AccessKeySecret}
	f.handler = func(_ string, _ map[string]string, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"RequestId":"x"}`))
	}
	c, _ := newTestClient(t, f)
	if _, err := c.ListCdtInternetTraffic(context.Background(), testCreds); !errors.Is(err, ErrNoTrafficDetails) {
		t.Fatalf("没有 TrafficDetails 应报 ErrNoTrafficDetails,得到 %v", err)
	}
}

func TestClassOfTreatsHongKongAsInternational(t *testing.T) {
	if ClassOf("cn-hongkong") != ClassInternational {
		t.Fatal("cn-hongkong 应归国际")
	}
	if ClassOf("cn-hangzhou") != ClassChina {
		t.Fatal("cn-hangzhou 应归内地")
	}
	if ClassOf("us-west-1") != ClassInternational {
		t.Fatal("us-west-1 应归国际")
	}
	if ClassOf("cn") != ClassInternational {
		t.Fatal("不足三位的串不能当内地")
	}
}

func TestRetriesOnlyRetryableFailures(t *testing.T) {
	t.Run("503 重试后成功", func(t *testing.T) {
		f := &fakeAliyun{t: t, secret: testCreds.AccessKeySecret}
		f.handler = func(_ string, _ map[string]string, w http.ResponseWriter) {
			if f.calls.Load() < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"Code":"ServiceUnavailable","Message":"busy"}`))
				return
			}
			_, _ = w.Write([]byte(`{"InstanceStatuses":{"InstanceStatus":[{"InstanceId":"i-abc","Status":"Running"}]}}`))
		}
		c, _ := newTestClient(t, f)
		st, err := c.DescribeInstanceStatus(context.Background(), testCreds, "cn-hongkong", "i-abc")
		if err != nil || st != StatusRunning {
			t.Fatalf("status=%v err=%v", st, err)
		}
		if f.calls.Load() != 3 {
			t.Fatalf("应重试到第三次成功,实际调用 %d 次", f.calls.Load())
		}
	})
	t.Run("403 不重试", func(t *testing.T) {
		f := &fakeAliyun{t: t, secret: testCreds.AccessKeySecret}
		f.handler = func(_ string, _ map[string]string, w http.ResponseWriter) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"Code":"Forbidden.RAM","Message":"no permission for ` + testCreds.AccessKeyID + `","RequestId":"r2"}`))
		}
		c, _ := newTestClient(t, f)
		_, err := c.DescribeInstanceStatus(context.Background(), testCreds, "cn-hongkong", "i-abc")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != "Forbidden.RAM" {
			t.Fatalf("应得到 APIError Forbidden.RAM,得到 %v", err)
		}
		if f.calls.Load() != 1 {
			t.Fatalf("4xx 不该重试,实际调用 %d 次", f.calls.Load())
		}
		if strings.Contains(err.Error(), testCreds.AccessKeyID) {
			t.Fatalf("错误信息里泄露了 AccessKey ID: %v", err)
		}
		if !strings.Contains(err.Error(), MaskAccessKeyID(testCreds.AccessKeyID)) {
			t.Fatalf("错误信息应保留脱敏后的 ID 便于分辨账号: %v", err)
		}
	})
}

func TestDescribeInstanceReadsIPsAndStoppedMode(t *testing.T) {
	f := &fakeAliyun{t: t, secret: testCreds.AccessKeySecret}
	f.handler = func(action string, form map[string]string, w http.ResponseWriter) {
		if action != "DescribeInstances" || form["InstanceIds"] != `["i-abc"]` {
			t.Errorf("参数不对: %s %v", action, form)
		}
		_, _ = w.Write([]byte(`{"Instances":{"Instance":[{
			"InstanceId":"i-abc","InstanceName":"hk","RegionId":"cn-hongkong","ZoneId":"cn-hongkong-b",
			"Status":"Stopped","StoppedMode":"StopCharging","InstanceChargeType":"PostPaid","SpotStrategy":"NoSpot",
			"PublicIpAddress":{"IpAddress":[]},"EipAddress":{"IpAddress":"8.8.8.8","AllocationId":"eip-1"}}]}}`))
	}
	c, _ := newTestClient(t, f)
	inst, err := c.DescribeInstance(context.Background(), testCreds, "cn-hongkong", "i-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !inst.HasEIP() || inst.EffectivePublicIP() != "8.8.8.8" || inst.StoppedMode != "StopCharging" || inst.Status != StatusStopped {
		t.Fatalf("解析结果 = %+v", inst)
	}
}

func TestMissingInstanceIsAnError(t *testing.T) {
	f := &fakeAliyun{t: t, secret: testCreds.AccessKeySecret}
	f.handler = func(_ string, _ map[string]string, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"InstanceStatuses":{"InstanceStatus":[]}}`))
	}
	c, _ := newTestClient(t, f)
	if _, err := c.DescribeInstanceStatus(context.Background(), testCreds, "cn-hongkong", "i-gone"); err == nil {
		t.Fatal("实例不存在时阿里云回空列表,面板必须把它翻成错误")
	}
}

func TestStopInstanceRejectsUnknownMode(t *testing.T) {
	c := New()
	if err := c.StopInstance(context.Background(), testCreds, "cn-hongkong", "i-abc", "Whatever"); !errors.Is(err, ErrUnknownStoppedMode) {
		t.Fatalf("err = %v", err)
	}
	if err := c.StartInstance(context.Background(), testCreds, "cn-hongkong", "abc"); err == nil {
		t.Fatal("不以 i- 开头的实例 ID 应被拒绝")
	}
}

func TestNoStockIsRecognised(t *testing.T) {
	err := &APIError{Action: "StartInstance", Code: "OperationDenied.NoStock", Message: "no stock"}
	if !IsNoStock(err) {
		t.Fatal("应识别 NoStock")
	}
	if IsIncorrectStatus(err) {
		t.Fatal("NoStock 不是 IncorrectInstanceStatus")
	}
}

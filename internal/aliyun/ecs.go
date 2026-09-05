package aliyun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ECS 那一侧:查状态、查实例、开机、关机。

const ecsVersion = "2014-05-26"

func ecsHost(region string) string { return "ecs." + region + ".aliyuncs.com" }

// InstanceStatus 是 ECS 实例的运行状态,取值照阿里云的写法。
type InstanceStatus string

const (
	StatusRunning  InstanceStatus = "Running"
	StatusStopped  InstanceStatus = "Stopped"
	StatusStarting InstanceStatus = "Starting"
	StatusStopping InstanceStatus = "Stopping"
	StatusPending  InstanceStatus = "Pending"
	// StatusUnknown 是"还没查过"或"查不到"。它与 Stopped 必须严格分开:
	// 前者说明不了任何事,后者才是可以据此开机的事实。
	StatusUnknown InstanceStatus = ""
)

// Transient 表示实例正在变化(开机中 / 关机中 / 创建中),此时不该再下命令。
func (s InstanceStatus) Transient() bool {
	return s == StatusStarting || s == StatusStopping || s == StatusPending
}

// Label 是给人看的状态名。
func (s InstanceStatus) Label() string {
	switch s {
	case StatusRunning:
		return "运行中"
	case StatusStopped:
		return "已停止"
	case StatusStarting:
		return "启动中"
	case StatusStopping:
		return "停止中"
	case StatusPending:
		return "创建中"
	}
	return "未知"
}

// StoppedMode 是关机时的计费模式。
type StoppedMode string

const (
	// KeepCharging 普通停机:实例继续计费,IP 不变。
	KeepCharging StoppedMode = "KeepCharging"
	// StopCharging 节省停机:不计算力费用,但**系统分配的公网 IP 会被释放、
	// 开机后换一个**;EIP 不受影响。开机还可能因为库存不足失败(NoStock)。
	StopCharging StoppedMode = "StopCharging"
)

// ErrUnknownStoppedMode 表示停机模式取值非法。
var ErrUnknownStoppedMode = errors.New("停机模式只能是 KeepCharging 或 StopCharging")

// ParseStoppedMode 解析停机模式,空串按 StopCharging(V17 定的默认值)。
func ParseStoppedMode(raw string) (StoppedMode, error) {
	switch m := StoppedMode(strings.TrimSpace(raw)); m {
	case "":
		return StopCharging, nil
	case KeepCharging, StopCharging:
		return m, nil
	}
	return "", ErrUnknownStoppedMode
}

// Label 是给人看的模式名。
func (m StoppedMode) Label() string {
	if m == StopCharging {
		return "节省停机"
	}
	return "普通停机"
}

// Instance 是 DescribeInstances 里我们关心的那几个字段。
type Instance struct {
	InstanceID   string         `json:"instance_id"`
	InstanceName string         `json:"instance_name"`
	RegionID     string         `json:"region_id"`
	ZoneID       string         `json:"zone_id"`
	InstanceType string         `json:"instance_type"`
	Status       InstanceStatus `json:"status"`
	// StoppedMode 是实例**当前**的停机计费模式(Stopped 时才有意义)。
	// 阿里云对不满足经济模式条件的实例会**静默**退回普通停机,
	// 所以停完要读一次这个字段而不是相信自己发了什么。
	StoppedMode string `json:"stopped_mode"`
	// ChargeType 是 PostPaid(按量)或 PrePaid(包年包月);后者不支持节省停机。
	ChargeType string `json:"charge_type"`
	// SpotStrategy 非 NoSpot 表示抢占式实例 —— 会被阿里云回收,保活就是给它准备的。
	SpotStrategy string `json:"spot_strategy"`
	// PublicIP 是系统分配的公网 IP(节省停机会换),EIP 是弹性公网 IP(不会换)。
	// 两者最多有一个非空。
	PublicIP string `json:"public_ip"`
	EIP      string `json:"eip"`
	OSName   string `json:"os_name"`
}

// EffectivePublicIP 是用户会连到的那个地址:有 EIP 用 EIP,否则用系统分配的。
func (i Instance) EffectivePublicIP() string {
	if i.EIP != "" {
		return i.EIP
	}
	return i.PublicIP
}

// HasEIP 表示这台实例挂着弹性公网 IP,节省停机不会换它的地址。
func (i Instance) HasEIP() bool { return i.EIP != "" }

// DescribeInstanceStatus 只查状态,比 DescribeInstances 轻,给每轮轮询用。
//
// 实例不存在时阿里云**不报错**,返回空列表 —— 这里把它翻成一个明确的错误,
// 不然一台被释放掉的实例会永远显示"未知",而管理员看不出为什么。
func (c *Client) DescribeInstanceStatus(ctx context.Context, creds Credentials, region, instanceID string) (InstanceStatus, error) {
	if err := checkInstanceID(instanceID); err != nil {
		return StatusUnknown, err
	}
	result, err := c.call(ctx, creds, ecsHost(region), region, ecsVersion, "DescribeInstanceStatus",
		map[string]string{"InstanceId.1": instanceID})
	if err != nil {
		return StatusUnknown, err
	}
	for _, it := range sliceOf(nested(result, "InstanceStatuses")) {
		obj, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if stringOf(obj["InstanceId"]) == instanceID {
			if s := InstanceStatus(stringOf(obj["Status"])); s != "" {
				return s, nil
			}
		}
	}
	return StatusUnknown, fmt.Errorf("阿里云在 %s 找不到实例 %s(已释放,或区域填错了)", region, instanceID)
}

// DescribeInstance 查一台实例的详情(状态、IP、EIP、计费方式、停机模式)。
func (c *Client) DescribeInstance(ctx context.Context, creds Credentials, region, instanceID string) (Instance, error) {
	if err := checkInstanceID(instanceID); err != nil {
		return Instance{}, err
	}
	ids, _ := json.Marshal([]string{instanceID})
	list, err := c.describeInstances(ctx, creds, region, map[string]string{"InstanceIds": string(ids)})
	if err != nil {
		return Instance{}, err
	}
	for _, i := range list {
		if i.InstanceID == instanceID {
			return i, nil
		}
	}
	return Instance{}, fmt.Errorf("阿里云在 %s 找不到实例 %s(已释放,或区域填错了)", region, instanceID)
}

// ListInstances 列出一个区域里的全部实例,给节点表单里「从账号拉取实例列表」用。
// 只取第一页 100 台 —— 本项目的用户不会有超过这个数的机器。
func (c *Client) ListInstances(ctx context.Context, creds Credentials, region string) ([]Instance, error) {
	return c.describeInstances(ctx, creds, region, map[string]string{"PageSize": "100"})
}

func (c *Client) describeInstances(ctx context.Context, creds Credentials, region string, extras map[string]string) ([]Instance, error) {
	result, err := c.call(ctx, creds, ecsHost(region), region, ecsVersion, "DescribeInstances", extras)
	if err != nil {
		return nil, err
	}
	items := sliceOf(nested(result, "Instances"))
	out := make([]Instance, 0, len(items))
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok {
			continue
		}
		inst := Instance{
			InstanceID:   stringOf(obj["InstanceId"]),
			InstanceName: stringOf(obj["InstanceName"]),
			RegionID:     stringOf(obj["RegionId"]),
			ZoneID:       stringOf(obj["ZoneId"]),
			InstanceType: stringOf(obj["InstanceType"]),
			Status:       InstanceStatus(stringOf(obj["Status"])),
			StoppedMode:  stringOf(obj["StoppedMode"]),
			ChargeType:   stringOf(obj["InstanceChargeType"]),
			SpotStrategy: stringOf(obj["SpotStrategy"]),
			OSName:       stringOf(obj["OSName"]),
		}
		if ips := sliceOf(nested(obj, "PublicIpAddress")); len(ips) > 0 {
			inst.PublicIP = stringOf(ips[0])
		}
		inst.EIP = stringOf(nested(obj, "EipAddress", "IpAddress"))
		out = append(out, inst)
	}
	return out, nil
}

// StartInstance 开机。异步:返回成功只表示指令已受理,状态要另外查。
func (c *Client) StartInstance(ctx context.Context, creds Credentials, region, instanceID string) error {
	if err := checkInstanceID(instanceID); err != nil {
		return err
	}
	_, err := c.call(ctx, creds, ecsHost(region), region, ecsVersion, "StartInstance",
		map[string]string{"InstanceId": instanceID})
	return err
}

// StopInstance 关机。异步,同上。
//
// 不传 ForceStop:强制停机等于拔电源,而这台机器上 sing-box 的计数器
// 正等着最后一次同步 —— 调用方在停之前会尽力同步一次,这里只做正常关机。
func (c *Client) StopInstance(ctx context.Context, creds Credentials, region, instanceID string, mode StoppedMode) error {
	if err := checkInstanceID(instanceID); err != nil {
		return err
	}
	if mode != KeepCharging && mode != StopCharging {
		return ErrUnknownStoppedMode
	}
	_, err := c.call(ctx, creds, ecsHost(region), region, ecsVersion, "StopInstance",
		map[string]string{"InstanceId": instanceID, "StoppedMode": string(mode)})
	return err
}

// checkInstanceID 挡掉空值与明显写错的 ID。阿里云的实例 ID 都是 `i-` 开头。
func checkInstanceID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("实例 ID 不能为空")
	}
	if !strings.HasPrefix(id, "i-") || strings.ContainsAny(id, " \t\r\n\"'") {
		return fmt.Errorf("实例 ID %q 不像阿里云的实例 ID(应以 i- 开头)", id)
	}
	return nil
}

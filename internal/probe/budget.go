package probe

import (
	"hash/fnv"
	"sync"
	"time"
)

// spendLedger 记最近一分钟对第三方目标的实际探测尝试(ICMP 按包、TCP 握手近似按次、TCP 建连按次),
// 有全站与单目标两个口径。
//
// 预算按「打算做几次」先占,跑完之后按「实际做了几次」结算:节点正忙让路、SSH 连不上、DNS 没解析出来,
// 都一个包都没发,占的额度全退回去。只在内存里:面板重启后从零记起 —— 重启后各序列按
// 「上次探测 + 间隔」接着排,不会在启动那一刻把第三方目标集中打一遍。
type spendLedger struct {
	mu      sync.Mutex
	entries []spend
	nextID  int64
}

type spend struct {
	id     int64
	at     time.Time
	public int64
	cost   int
}

const spendWindow = time.Minute

// ticksPerMinute 是一分钟里有几个节拍;单目标的每拍上限 = 每分钟上限 ÷ 它。
var ticksPerMinute = int(spendWindow / OutboundTick)

func (l *spendLedger) pruneLocked(now time.Time) {
	cut := now.Add(-spendWindow)
	i := 0
	for i < len(l.entries) && !l.entries[i].at.After(cut) {
		i++
	}
	l.entries = l.entries[i:]
}

// usage 返回最近一分钟的全站合计与按目标的合计。
func (l *spendLedger) usage(now time.Time) (int, map[int64]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	total, by := 0, map[int64]int{}
	for _, e := range l.entries {
		total += e.cost
		by[e.public] += e.cost
	}
	return total, by
}

// add 先占一笔额度,返回结算用的编号。
func (l *spendLedger) add(now time.Time, public int64, cost int) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	l.entries = append(l.entries, spend{id: l.nextID, at: now, public: public, cost: cost})
	return l.nextID
}

// settle 把一笔预占改成实际发生的次数(0 = 全部退回)。
func (l *spendLedger) settle(id int64, actual int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.entries {
		if l.entries[i].id == id {
			l.entries[i].cost = actual
			return
		}
	}
}

// admission 是一个节拍里的放行判断:全站预算、单目标每分钟限流、单目标每拍上限。
// 同一个窗口里第一次探测一个目标时总是放行(哪怕它一次的尝试数就超过了单目标上限),
// 不然这种目标会被永远饿着,而管理员看到的只是它一直没有数据。
type admission struct {
	budget  int
	total   int
	byPub   map[int64]int
	perTick map[int64]int
}

func (a *admission) allow(public int64, cost, perMin int) bool {
	if a.total > 0 && a.total+cost > a.budget {
		return false
	}
	if perMin > 0 {
		if a.byPub[public] > 0 && a.byPub[public]+cost > perMin {
			return false
		}
		tickCap := (perMin + ticksPerMinute - 1) / ticksPerMinute
		if a.perTick[public] > 0 && a.perTick[public]+cost > tickCap {
			return false
		}
	}
	a.total += cost
	a.byPub[public] += cost
	a.perTick[public] += cost
	return true
}

// attemptsOf 是一个目标一轮打算做几次实际尝试(预算按它预占)。
func attemptsOf(p PublicTarget) int {
	if p.Protocol == ProtoICMP || p.TCPMode == TCPSyn {
		if p.Count > 0 {
			return p.Count
		}
		return PublicDefaultCount
	}
	return 1
}

// spread 把(机器, 目标)映射成一个稳定的伪随机数,用来错开各机器打同一个第三方目标的时刻。
func spread(nodeID, publicID int64, salt int64) uint32 {
	h := fnv.New32a()
	var b [24]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(nodeID >> (8 * i))
		b[8+i] = byte(publicID >> (8 * i))
		b[16+i] = byte(salt >> (8 * i))
	}
	_, _ = h.Write(b[:])
	return h.Sum32()
}

// firstPhase 是第三方目标第一次到期前的错峰时长:在 min(间隔, 5 分钟) 里按(机器, 目标)均匀散开。
// 新建一个「全部节点」的第三方目标时,各机器不会在同一个节拍里一起去打它。
func firstPhase(nodeID, publicID int64, interval time.Duration) time.Duration {
	span := interval
	if span > 5*time.Minute {
		span = 5 * time.Minute
	}
	if span <= 0 {
		return 0
	}
	return time.Duration(spread(nodeID, publicID, 0)%uint32(span/time.Second)) * time.Second
}

// jitter 是第三方目标每次排下一轮时的抖动(间隔的 ±5%),让各机器的相位慢慢散开、不会一直撞在一起。
func jitter(nodeID, publicID int64, interval time.Duration, start time.Time) time.Duration {
	width := int64(interval / time.Second / 10)
	if width <= 0 {
		return 0
	}
	period := start.Unix() / int64(interval/time.Second)
	off := int64(spread(nodeID, publicID, period)) % (width + 1)
	return time.Duration(off-width/2) * time.Second
}

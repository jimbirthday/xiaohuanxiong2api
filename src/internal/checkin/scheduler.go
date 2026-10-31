package checkin

import (
	"context"
	"hash/fnv"
	"time"
)

// scheduleWakeSegment 把任意长的等待切成不超过 30 秒的墙钟分段。
// 系统睡眠会冻结计时器的单调时钟；分段后醒来重新读墙钟，最多延误一段。
const scheduleWakeSegment = 30 * time.Second

// nextEvent 重新计算下一次应唤醒的时刻：每日签到时刻与最早失败重试取更早者。
func (s *Service) nextEvent(now time.Time) time.Time {
	next := s.nextDaily(now)
	if retry, ok := s.earliestRetry(now); ok && retry.Before(next) {
		next = retry
	}
	return next
}

// waitUntil 分段等待到 wall-clock 目标时刻。每段结束都重新读取注入时钟，
// 因此测试时钟跳变或机器休眠后不会继续傻等原 timer。
func (s *Service) waitUntil(ctx context.Context, next time.Time) bool {
	for {
		now := s.clock()
		if !next.After(now) {
			return ctx.Err() == nil
		}
		if !s.wait(ctx, nextWakeDelay(now, next, s.wakeSegment)) {
			return false
		}
	}
}

func nextWakeDelay(now, next time.Time, segment time.Duration) time.Duration {
	if !next.After(now) {
		return 0
	}
	delay := next.Sub(now)
	if segment > 0 && delay > segment {
		return segment
	}
	return delay
}

// accountJitter 用 FNV-1a(accountID|date) 生成稳定的启动抖动。
// 同一账号同一天每次触发得到同一延迟，避免重启后抖动随机化形成突发。
func accountJitter(accountID, day string, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(accountID))
	_, _ = hash.Write([]byte{'|'})
	_, _ = hash.Write([]byte(day))
	return time.Duration(hash.Sum64() % uint64(max))
}

package provider

import (
	"fmt"
	"math"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func ValidateVideoKeyframes(frames []media.VideoKeyframe, duration int) error {
	if len(frames) > media.MaxVideoKeyframes {
		return fmt.Errorf("%w: keyframes 最多 %d 个", ErrVideoParameterInvalid, media.MaxVideoKeyframes)
	}
	slots := make(map[int]int, len(frames))
	for i, frame := range frames {
		if strings.TrimSpace(frame.ImageURL) == "" {
			return fmt.Errorf("%w: keyframes[%d].image 不能为空", ErrVideoParameterInvalid, i)
		}
		timestamp := frame.TimestampSeconds
		if math.IsNaN(timestamp) || math.IsInf(timestamp, 0) || timestamp <= 0 || timestamp >= float64(duration) {
			return fmt.Errorf("%w: keyframes[%d].timestamp_s 必须大于 0 且小于 duration（%d 秒）", ErrVideoParameterInvalid, i, duration)
		}
		slot := int(math.Round(timestamp * 3))
		if previous, exists := slots[slot]; exists {
			return fmt.Errorf("%w: keyframes[%d] 与 keyframes[%d] 的时间落在同一 1/3 秒位置", ErrVideoParameterInvalid, previous, i)
		}
		slots[slot] = i
	}
	return nil
}

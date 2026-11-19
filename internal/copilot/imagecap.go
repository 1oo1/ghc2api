package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
)

const omittedHistoryImage = `{"type":"text","text":"[An earlier image was omitted to stay within the model's image limit.]"}`

var imageCapMessage = regexp.MustCompile(`(?i)^too many images: maximum allowed for model (\S+) is ([0-9]+)(?:, got [0-9]+)?\s*$`)

func (s *Service) imageCap(model string) int {
	s.imageCapsMu.Lock()
	defer s.imageCapsMu.Unlock()
	return s.imageCaps[model]
}

func (s *Service) learnImageCap(model string, limit int) int {
	s.imageCapsMu.Lock()
	defer s.imageCapsMu.Unlock()
	if old := s.imageCaps[model]; old > 0 && old < limit {
		return old
	}
	if s.imageCaps == nil {
		s.imageCaps = make(map[string]int)
	}
	s.imageCaps[model] = limit
	return limit
}

// learnRejectedImageCap 只根据上游明确拒绝学习上限；/models 中宣称的上限不一定实际执行。
func (s *Service) learnRejectedImageCap(res *http.Response, model string) int {
	if res.StatusCode != http.StatusBadRequest {
		return 0
	}
	var wire struct {
		Error struct{ Message string }
	}
	if json.Unmarshal(peekErrorBody(res), &wire) != nil {
		return 0
	}
	match := imageCapMessage.FindStringSubmatch(wire.Error.Message)
	if match == nil || match[1] != model {
		return 0
	}
	limit, err := strconv.Atoi(match[2])
	if err != nil || limit <= 0 {
		return 0
	}
	return s.learnImageCap(model, limit)
}

func (s *Service) retryGeminiImageCap(ctx context.Context, res *http.Response, body []byte, model string,
	send func([]byte) (*http.Response, error)) (*http.Response, error) {
	limit := s.learnRejectedImageCap(res, model)
	fitted, removed := fitGeminiImages(body, limit)
	if removed == 0 {
		return res, nil
	}
	s.log(ctx, slog.LevelWarn, "上游拒绝图片超限，替换最早的历史图片后重试", "model", model, "image_limit", limit, "omitted_images", removed)
	_ = res.Body.Close()
	retry, err := send(fitted)
	if err == nil {
		s.learnRejectedImageCap(retry, model)
	}
	return retry, err
}

// fitGeminiImages 只替换 Chat Completions 的 messages[*].content 中的图片部件，其余字节不变。
// 如果保留最后一条 user 消息仍无法满足上限，则不做任何修改。body 已由 handleGeneration 完整校验。
func fitGeminiImages(body []byte, limit int) ([]byte, int) {
	if limit <= 0 {
		return body, 0
	}
	messages := lastImageCapMember(body, "messages")
	total, protected, latestUser := 0, 0, -1
	eachObjectAt(messages.value, func(message []byte, at int) bool {
		count := 0
		chatImageParts(message, func(_, _ int) bool { count++; return true })
		total += count
		role := lastImageCapMember(message, "role").value
		var decoded [4]byte
		if firstByte(role) == '"' && string(decodeASCIIString(role, decoded[:])) == "user" {
			latestUser, protected = at, count
		}
		return true
	})
	over := total - limit
	if over <= 0 || protected > limit {
		return body, 0
	}
	visitOmitted := func(visit func(start, end int)) {
		remaining := over
		eachObjectAt(messages.value, func(message []byte, at int) bool {
			if at == latestUser {
				return true
			}
			chatImageParts(message, func(start, end int) bool {
				offset := messages.valueAt + at
				visit(offset+start, offset+end)
				remaining--
				return remaining > 0
			})
			return remaining > 0
		})
	}
	size := len(body)
	visitOmitted(func(start, end int) {
		size += len(omittedHistoryImage) - (end - start)
	})
	if size > maxRequestBody {
		return body, 0
	}
	out, last := make([]byte, 0, size), 0
	visitOmitted(func(start, end int) {
		out = append(out, body[last:start]...)
		out = append(out, omittedHistoryImage...)
		last = end
	})
	return append(out, body[last:]...), over
}

func chatImageParts(message []byte, visit func(start, end int) bool) {
	content := lastImageCapMember(message, "content")
	eachObjectAt(content.value, func(part []byte, at int) bool {
		kind := lastImageCapMember(part, "type").value
		if firstByte(kind) == '"' {
			var decoded [11]byte
			switch string(decodeASCIIString(kind, decoded[:])) {
			case "image_url", "input_image":
				return visit(content.valueAt+at, content.valueAt+at+len(part))
			}
		}
		return true
	})
}

func lastImageCapMember(object []byte, name string) (last jsonMember) {
	var decoded [8]byte
	eachJSONMember(object, func(key, value []byte, at int) {
		if string(decodeASCIIString(key, decoded[:])) == name {
			last.value, last.valueAt = bytes.TrimRight(value, " \t\r\n"), at
		}
	})
	return last
}

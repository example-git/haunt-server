package serverextension

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var sessionPattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)
var voicePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{2,64}$`)

func splitText(text string) []string {
	var chunks []string
	start, size := 0, 0
	for offset, character := range text {
		count := utf8.RuneLen(character)
		if size+count > 300 {
			chunks = append(chunks, text[start:offset])
			start, size = offset, 0
		}
		size += count
	}
	if start < len(text) {
		chunks = append(chunks, text[start:])
	}
	return chunks
}

func synthesizeTikTok(ctx context.Context, client *http.Client, endpoint, token string, prompt SpeechRequest) ([]byte, error) {
	if !utf8.ValidString(prompt.Text) || strings.TrimSpace(prompt.Text) == "" {
		return nil, ErrInvalid
	}
	var audio []byte
	for _, text := range splitText(prompt.Text) {
		form := url.Values{"text_speaker": {prompt.Voice}, "req_text": {text}, "speaker_map_type": {"0"}, "aid": {"1233"}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/media/api/text/speech/invoke/", strings.NewReader(form.Encode()))
		if err != nil {
			return nil, ErrInvalid
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("User-Agent", "com.zhiliaoapp.musically/2024302010 (Linux; U; Android 13; en_US; Pixel 7; Build/TD1A.220804.031; Cronet/58.0.2991.0)")
		request.Header.Set("Cookie", "sessionid="+token)
		request.Header.Set("x-common-params-v2", "version_code=34.3.2&app_name=musical_ly&channel=googleplay&device_platform=android&device_type=Pixel+7&os_version=13")
		response, err := client.Do(request)
		if err != nil {
			return nil, ErrUpstream
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 12*1024*1024+1))
		response.Body.Close()
		if response.StatusCode == 401 || response.StatusCode == 403 {
			return nil, ErrCredentials
		}
		if response.StatusCode == 429 {
			return nil, ErrBusy
		}
		if response.StatusCode != 200 || readErr != nil || len(data) > 12*1024*1024 {
			return nil, ErrUpstream
		}
		var result struct {
			Status *int `json:"status_code"`
			Data   struct {
				Audio string `json:"v_str"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &result) != nil || result.Status == nil || *result.Status != 0 || result.Data.Audio == "" {
			return nil, ErrUpstream
		}
		chunk, err := base64.StdEncoding.Strict().DecodeString(result.Data.Audio)
		if err != nil || len(chunk) == 0 || len(audio)+len(chunk) > 8*1024*1024 {
			return nil, ErrUpstream
		}
		audio = append(audio, chunk...)
	}
	return audio, nil
}

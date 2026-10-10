package wbot

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const maxMedia = 100 << 20
const cdnEndpoint = "https://novac2c.cdn.weixin.qq.com/c2c"

type MediaPointer struct {
	Query      string `json:"encrypt_query_param,omitempty"`
	Key        string `json:"aes_key,omitempty"`
	Encryption int    `json:"encrypt_type,omitempty"`
	URL        string `json:"full_url,omitempty"`
}

type MediaItem struct {
	Media    *MediaPointer `json:"media,omitempty"`
	Name     string        `json:"file_name,omitempty"`
	ImageKey string        `json:"aeskey,omitempty"`
}

type Attachment struct {
	Kind  string `json:"kind"`
	Name  string `json:"name,omitempty"`
	Path  string `json:"path,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
	Error string `json:"error,omitempty"`
}

// ECB and PKCS#7 here implement the iLink wire format.
func encryptMedia(plain, key []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, errors.New("媒体密钥长度无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := make([]byte, len(plain)+pad)
	copy(data, plain)
	for i := len(plain); i < len(data); i++ {
		data[i] = byte(pad)
	}
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Encrypt(data[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	return data, nil
}

func decryptMedia(data, key []byte) ([]byte, error) {
	if len(key) != 16 || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("媒体密文或密钥格式无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(plain[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize {
		return nil, errors.New("媒体填充校验失败")
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, errors.New("媒体填充校验失败")
		}
	}
	return plain[:len(plain)-pad], nil
}

func mediaKey(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("媒体密钥编码无效")
	}
	if len(b) == 32 {
		b, err = hex.DecodeString(string(b))
	}
	if err != nil || len(b) != 16 {
		return nil, errors.New("媒体密钥长度无效")
	}
	return b, nil
}

func validateCDN(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" {
		return errors.New("媒体地址无效")
	}
	if u.Scheme == "https" && strings.HasSuffix(u.Hostname(), ".weixin.qq.com") {
		return nil
	}
	if u.Scheme == "http" && loopbackHost(u.Hostname()) {
		return nil
	}
	return errors.New("媒体地址需要使用腾讯微信 HTTPS 域名")
}

func (c *Client) Download(ctx context.Context, pointer *MediaPointer, imageKey string, plainImage bool) ([]byte, error) {
	if pointer == nil {
		return nil, errors.New("消息未提供媒体下载信息")
	}
	address := pointer.URL
	if address == "" {
		if pointer.Query == "" {
			return nil, errors.New("消息未提供媒体下载参数")
		}
		address = cdnEndpoint + "/download?" + url.Values{"encrypted_query_param": {pointer.Query}}.Encode()
	}
	if err := validateCDN(address); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("媒体下载请求无效")
	}
	resp, err := httpClient(2 * time.Minute).Do(req)
	if err != nil {
		return nil, errors.New("媒体下载连接失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("媒体下载 HTTP 状态码 %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMedia+aes.BlockSize+1))
	if err != nil || len(data) > maxMedia+aes.BlockSize {
		return nil, errors.New("媒体读取失败或超过 100 MB")
	}
	var key []byte
	if imageKey != "" {
		key, err = hex.DecodeString(imageKey)
	} else if pointer.Key != "" {
		key, err = mediaKey(pointer.Key)
	} else if plainImage {
		return data, nil
	} else {
		return nil, errors.New("媒体消息缺少解密密钥")
	}
	if err != nil {
		return nil, errors.New("媒体密钥格式无效")
	}
	return decryptMedia(data, key)
}

func attachmentSource(item Item) (string, *MediaItem) {
	switch item.Type {
	case 2:
		return "image", item.Image
	case 4:
		return "file", item.File
	case 5:
		return "video", item.Video
	case 3:
		if item.Voice != nil && item.Voice.Media != nil {
			return "voice", &MediaItem{Media: item.Voice.Media}
		}
	}
	return "", nil
}

func (c *Client) SaveAttachment(ctx context.Context, dir, eventID string, index int, item Item) Attachment {
	kind, source := attachmentSource(item)
	out := Attachment{Kind: kind}
	if source == nil {
		out.Error = "消息未提供附件信息"
		return out
	}
	name := filepath.Base(source.Name)
	if name == "." || name == "" || name == string(filepath.Separator) {
		name = map[string]string{"image": "image.bin", "video": "video.mp4", "file": "attachment.bin", "voice": "voice.silk"}[kind]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if len(name) > 120 {
		name = "attachment" + filepath.Ext(name)
	}
	out.Name = name
	data, err := c.Download(ctx, source.Media, source.ImageKey, kind == "image")
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if kind == "image" && strings.HasSuffix(name, ".bin") {
		switch http.DetectContentType(data) {
		case "image/png":
			name = "image.png"
		case "image/jpeg":
			name = "image.jpg"
		case "image/gif":
			name = "image.gif"
		case "image/webp":
			name = "image.webp"
		}
		out.Name = name
	}
	if err = privateDir(dir); err != nil {
		out.Error = "无法创建附件目录"
		return out
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d-%s", eventID, index, name))
	if err = os.WriteFile(path, data, 0600); err != nil {
		out.Error = "无法保存附件"
		return out
	}
	out.Path = path
	out.Bytes = len(data)
	return out
}

func (c *Client) Upload(ctx context.Context, to, kind, path string) (any, error) {
	mediaType := map[string]int{"image": 1, "video": 2, "file": 3}[kind]
	itemType := map[string]int{"image": 2, "video": 5, "file": 4}[kind]
	if mediaType == 0 {
		return nil, errors.New("发送附件仅支持 image、file、video")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("无法打开附件文件")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > maxMedia {
		return nil, errors.New("附件需要是 100 MB 以内的普通文件")
	}
	plain, err := io.ReadAll(io.LimitReader(file, maxMedia+1))
	if err != nil || len(plain) > maxMedia {
		return nil, errors.New("附件读取失败或超过 100 MB")
	}
	key := make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	encrypted, err := encryptMedia(plain, key)
	if err != nil {
		return nil, err
	}
	digest := md5.Sum(plain) // Required by the iLink upload protocol, not used for authentication.
	filekey := randomID()
	var upload struct {
		URL   string `json:"upload_full_url"`
		Query string `json:"upload_param"`
	}
	err = c.call(ctx, http.MethodPost, "getuploadurl", map[string]any{
		"filekey": filekey, "media_type": mediaType, "to_user_id": to, "rawsize": len(plain),
		"rawfilemd5": hex.EncodeToString(digest[:]), "filesize": len(encrypted), "no_need_thumb": true, "aeskey": hex.EncodeToString(key),
	}, &upload, true)
	if err != nil {
		return nil, err
	}
	address := upload.URL
	if address == "" {
		if upload.Query == "" {
			return nil, errors.New("微信接口未返回附件上传地址")
		}
		address = cdnEndpoint + "/upload?" + url.Values{"encrypted_query_param": {upload.Query}, "filekey": {filekey}}.Encode()
	}
	if err = validateCDN(address); err != nil {
		return nil, err
	}
	var pointer string
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err = wait(ctx, time.Duration(attempt)*time.Second); err != nil {
				return nil, err
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(encrypted))
		if err != nil {
			return nil, errors.New("附件上传地址无效")
		}
		request.Header.Set("Content-Type", "application/octet-stream")
		response, requestErr := httpClient(2 * time.Minute).Do(request)
		if requestErr != nil {
			continue
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if response.StatusCode >= 400 && response.StatusCode < 500 {
			return nil, fmt.Errorf("附件上传 HTTP 状态码 %d", response.StatusCode)
		}
		if response.StatusCode == 200 {
			pointer = response.Header.Get("x-encrypted-param")
			if pointer != "" {
				break
			}
		}
	}
	if pointer == "" {
		return nil, errors.New("附件上传未成功确认")
	}
	media := MediaPointer{Query: pointer, Key: base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(key))), Encryption: 1}
	details := map[string]any{"media": media}
	switch kind {
	case "file":
		details["file_name"] = filepath.Base(path)
		details["len"] = strconv.Itoa(len(plain))
	case "image":
		details["mid_size"] = len(encrypted)
	case "video":
		details["video_size"] = len(encrypted)
	}
	return map[string]any{"type": itemType, kind + "_item": details}, nil
}

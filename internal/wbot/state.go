package wbot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type Credentials struct {
	Token    string `json:"token"`
	Bot      string `json:"bot"`
	Owner    string `json:"owner"`
	Endpoint string `json:"endpoint"`
}

type Snapshot struct {
	Credentials Credentials       `json:"credentials"`
	Cursor      string            `json:"cursor"`
	ReplyTokens map[string]string `json:"reply_tokens"`
	Pending     []Event           `json:"pending"`
	Delivered   []string          `json:"delivered"`
}

type Journal struct {
	mu   sync.Mutex
	path string
	data Snapshot
}

func OpenJournal(dir string) (*Journal, error) {
	j := &Journal{path: filepath.Join(dir, "session.json")}
	j.data.ReplyTokens = map[string]string{}
	data, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &j.data); err != nil {
		return nil, fmt.Errorf("登录状态文件损坏：%w", err)
	}
	if j.data.ReplyTokens == nil {
		j.data.ReplyTokens = map[string]string{}
	}
	return j, nil
}

func cloneSnapshot(s Snapshot) Snapshot {
	b, _ := json.Marshal(s)
	var copy Snapshot
	_ = json.Unmarshal(b, &copy)
	return copy
}

func (j *Journal) Read() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return cloneSnapshot(j.data)
}

// Changes become visible to concurrent sends only once they are durable.
func (j *Journal) Update(fn func(*Snapshot)) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	next := cloneSnapshot(j.data)
	fn(&next)
	if err := writeJSON(j.path, next); err != nil {
		return err
	}
	j.data = next
	return nil
}

func AcquireProfile(dir string) (func(), error) {
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "process.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("这个配置已经有正在运行的 wbot，请先停止该进程")
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}

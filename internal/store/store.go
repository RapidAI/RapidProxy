// Package store 以文件形式保存各上游账号的凭据，一个账号一个 JSON 文件。
//
// 目录结构：
//
//	<dataDir>/accounts/<provider>/<credential-id>.json
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znsoftm/RapidProxy/internal/fsx"
	"github.com/znsoftm/RapidProxy/internal/upstream"
)

// Store 是凭据仓库。
type Store struct {
	root string
	mu   sync.RWMutex
}

// New 创建凭据仓库并确保目录存在。
func New(dataDir string) (*Store, error) {
	root := filepath.Join(dataDir, "accounts")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("创建凭据目录失败: %w", err)
	}
	return &Store{root: root}, nil
}

// Root 返回凭据根目录。
func (s *Store) Root() string { return s.root }

// safeName 防止账号 ID 越出目录。
func safeName(v string) string {
	v = strings.TrimSpace(v)
	replacer := strings.NewReplacer("\\", "_", "/", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	v = replacer.Replace(v)
	if v == "" || v == "." || v == ".." {
		return "unknown"
	}
	return v
}

func (s *Store) path(provider, id string) string {
	return filepath.Join(s.root, safeName(provider), safeName(id)+".json")
}

// Save 写入（或覆盖）一份凭据。
func (s *Store) Save(cred *upstream.Credential) error {
	if cred == nil || cred.Provider == "" || cred.ID == "" {
		return errors.New("凭据缺少 provider 或 id")
	}
	now := time.Now()
	if cred.CreatedAt.IsZero() {
		cred.CreatedAt = now
	}
	cred.UpdatedAt = now

	raw, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return fsx.AtomicWrite(s.path(cred.Provider, cred.ID), append(raw, '\n'), 0o600)
}

// Get 读取一份凭据。
func (s *Store) Get(provider, id string) (*upstream.Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readCredential(s.path(provider, id))
}

// List 列出凭据；provider 为空表示全部上游。
func (s *Store) List(provider string) ([]*upstream.Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var dirs []string
	if provider != "" {
		dirs = []string{filepath.Join(s.root, safeName(provider))}
	} else {
		entries, err := os.ReadDir(s.root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(s.root, e.Name()))
			}
		}
	}

	var out []*upstream.Credential
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			cred, err := readCredential(filepath.Join(dir, e.Name()))
			if err != nil || cred == nil {
				continue
			}
			if cred.Provider == "" {
				cred.Provider = filepath.Base(dir)
			}
			if cred.ID == "" {
				cred.ID = strings.TrimSuffix(e.Name(), ".json")
			}
			out = append(out, cred)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Delete 删除一份凭据。
func (s *Store) Delete(provider, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.Remove(s.path(provider, id))
}

// Count 返回凭据数量。
func (s *Store) Count(provider string) int {
	creds, err := s.List(provider)
	if err != nil {
		return 0
	}
	return len(creds)
}

func readCredential(path string) (*upstream.Credential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cred upstream.Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("凭据文件 %s 解析失败: %w", path, err)
	}
	return &cred, nil
}

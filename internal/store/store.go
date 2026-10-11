// Package store 以文件形式保存各上游账号的凭据，一个账号一个 JSON 文件。
//
// 目录结构：
//
//	<dataDir>/accounts/<provider>/<credential-id>.json
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znsoftm/RapidProxy/internal/upstream"
)

// Store 是凭据仓库。
type Store struct {
	root string
	mu   sync.RWMutex

	// DEK（数据加密密钥）惰性加载并缓存，避免每次读写都重新解封。
	// 用互斥锁 + 成功才缓存，而非 sync.Once：一旦 loadDEK 失败（例如 DPAPI
	// 暂时不可用），下次调用会重试，避免首次失败被永久缓存导致凭据再也无法保存。
	dekMu  sync.Mutex
	dekVal []byte
}

// New 创建凭据仓库并确保目录存在。
//
// accounts 目录及内部文件都是敏感数据，权限收紧到 0o700 / 0o600。
func New(dataDir string) (*Store, error) {
	root := filepath.Join(dataDir, "accounts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("创建凭据目录失败: %w", err)
	}
	return &Store{root: root}, nil
}

// Root 返回凭据根目录。
func (s *Store) Root() string { return s.root }

// 非法文件名字符，统一替换成下划线。
var unsafeChars = strings.NewReplacer(
	"\\", "_", "/", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
)

// safeName 防止账号 ID 越出目录。
//
// 替换非法字符会引入撞名：`a/b` 与 `a\b` 都变成 `a_b`，两个账号写同一个文件，
// 后保存的静默覆盖先保存的（令牌随之错乱）。因此只要发生了替换，
// 就在末尾追加原始值的短哈希，保证不同输入得到不同文件名。
//
// 注意这会改变含非法字符 ID 的落盘文件名：旧版写下的 `a_b.json` 读不到了，
// 所以读取与删除路径都要回退到 legacyName，旧文件会在下次 Save 时被迁移。
func safeName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "." || v == ".." {
		return "unknown"
	}
	if !strings.ContainsAny(v, `\\/*:"<>|`) {
		return v
	}
	mangled := unsafeChars.Replace(v)
	sum := sha256.Sum256([]byte(v))
	return mangled + "-" + hex.EncodeToString(sum[:6])
}

// legacyName 是旧版本的命名规则，仅用于兼容已存在的凭据文件。
//
// 复用 unsafeChars 而不是另写一份：两处规则一旦漂移（比如这里漏了某个字符），
// 回退路径就会算出一个既不等于新规则也不等于旧规则的名字，旧文件照样读不到。
func legacyName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "." || v == ".." {
		return "unknown"
	}
	return unsafeChars.Replace(v)
}

func (s *Store) path(provider, id string) string {
	return filepath.Join(s.root, safeName(provider), safeName(id)+".json")
}

// legacyPath 返回旧规则下的路径，可能并不存在。
func (s *Store) legacyPath(provider, id string) string {
	legacy := filepath.Join(s.root, legacyName(provider), legacyName(id)+".json")
	if legacy == s.path(provider, id) {
		return "" // 命名没变过，没有旧文件需要回退
	}
	return legacy
}

// Save 写入（或覆盖）一份凭据。
//
// 这里不做任何字段合并：调用方拿到的就是最终要落盘的内容。
// 早先试过「已存在则保留权重 / 暂停状态」，靠「字段是零值」来区分
// 重新登录与原地更新，但用户把权重改回 0（意为默认同权）时会被旧值
// 悄悄顶回去。合并规则放在登录流程里显式做，见 App.pollLogin。
func (s *Store) Save(cred *upstream.Credential) error {
	if cred == nil || cred.Provider == "" || cred.ID == "" {
		return errors.New("凭据缺少 provider 或 id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(cred)
}

// saveLocked 是 Save 的实现，调用方必须持有 s.mu 写锁。
func (s *Store) saveLocked(cred *upstream.Credential) error {
	if cred == nil || cred.Provider == "" || cred.ID == "" {
		return errors.New("凭据缺少 provider 或 id")
	}
	now := time.Now()
	if cred.CreatedAt.IsZero() {
		cred.CreatedAt = now
	}
	cred.UpdatedAt = now

	target := s.path(cred.Provider, cred.ID)
	if err := s.writeEncrypted(target, cred); err != nil {
		return err
	}
	// 命名规则升级过：把旧文件删掉，否则 List 会把同一个账号读出两份。
	if legacy := s.legacyPath(cred.Provider, cred.ID); legacy != "" {
		if _, err := os.Stat(legacy); err == nil {
			_ = os.Remove(legacy)
		}
	}
	return nil
}

// Get 读取一份凭据。
func (s *Store) Get(provider, id string) (*upstream.Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cred, err := s.readCred(s.path(provider, id))
	if err == nil {
		return cred, nil
	}
	// 旧版本用另一套命名规则写过含非法字符的 ID，回退去读一次，
	// 否则升级后这些账号会「凭空消失」。
	if legacy := s.legacyPath(provider, id); legacy != "" {
		if old, lerr := s.readCred(legacy); lerr == nil {
			return old, nil
		}
	}
	return nil, err
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
			cred, err := s.readCred(filepath.Join(dir, e.Name()))
			if err != nil || cred == nil {
				continue
			}
			if cred.Provider == "" {
				cred.Provider = filepath.Base(dir)
			}
			if cred.ID == "" {
				cred.ID = strings.TrimSuffix(e.Name(), ".json")
			}
			// 仍为空说明这份文件不可用（provider 目录名异常且 JSON 里也没写）。
			// 宁可不返回，也不要返回一个 ID 为空的账号——它在 pickCredential
			// 里会被当成正常候选，随后各种按 ID 的操作都会错位。
			if cred.Provider == "" || cred.ID == "" {
				continue
			}
			out = append(out, cred)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		// 登录时间可能完全相同（同一毫秒内批量登录），此时用 ID 兜底，
		// 否则 map/目录遍历顺序会让列表顺序在两次调用之间跳动。
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Delete 删除一份凭据。
func (s *Store) Delete(provider, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(provider, id))
	if err == nil {
		return nil
	}
	// 新路径没有、旧路径有：删的是旧规则下留下的文件。
	if legacy := s.legacyPath(provider, id); legacy != "" {
		if lerr := os.Remove(legacy); lerr == nil {
			return nil
		}
	}
	return err
}

// SavePreservingLocalFields 写入凭据，但保留已存在账号的权重与暂停状态。
//
// 用于重新登录同一账号的场景：凭据 ID 是 provider+uid，重新登录会算出
// 同一个 ID，Save 则是全量覆盖。如果不保留，用户在界面上配的权重和
// 「暂停」开关会在每次重新登录后被悄悄清成默认值。
//
// 这两件事必须显式区分，不能在 Save 里靠「字段是否为零值」去猜——
// 用户把权重改回 0（意为默认同权）时会被旧值顶回去，那种 bug 极难察觉。
func (s *Store) SavePreservingLocalFields(cred *upstream.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if old, err := s.readCred(s.path(cred.Provider, cred.ID)); err == nil && old != nil {
		cred.Weight = old.Weight
		cred.Paused = old.Paused
		if !old.CreatedAt.IsZero() {
			cred.CreatedAt = old.CreatedAt
		}
	}
	return s.saveLocked(cred)
}

// Count 返回凭据数量。
func (s *Store) Count(provider string) int {
	creds, err := s.List(provider)
	if err != nil {
		return 0
	}
	return len(creds)
}

package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/znsoftm/RapidProxy/internal/fsx"
	"github.com/znsoftm/RapidProxy/internal/upstream"
)

// dekFile 是「数据加密密钥」(DEK) 的受保护形态落盘位置，位于 accounts 目录下。
// 凭据本身用 AES-256-GCM 加密，DEK 则交给平台机制（Windows DPAPI / 其他平台密钥文件）保护，
// 这样即使凭据文件被单独拷贝走（备份、云同步、磁盘被盗），离线攻击者没有 DEK 也解不开。
const dekFile = ".dek"

// dek 惰性加载并缓存数据加密密钥：首次需要时生成，经平台机制保护后落盘。
// 仅在成功后才缓存；若 loadDEK 失败，下次调用会重试（见 Store.dekMu 注释）。
func (s *Store) dek() ([]byte, error) {
	s.dekMu.Lock()
	defer s.dekMu.Unlock()
	if s.dekVal != nil {
		return s.dekVal, nil
	}
	key, err := s.loadDEK()
	if err != nil {
		return nil, err
	}
	s.dekVal = key
	return key, nil
}

func (s *Store) loadDEK() ([]byte, error) {
	blobPath := filepath.Join(s.root, dekFile)
	if raw, err := os.ReadFile(blobPath); err == nil && len(raw) > 0 {
		if pt, err := platformUnprotect(raw); err == nil && len(pt) == 32 {
			return pt, nil
		}
		// 解开失败（例如文件被搬到了另一台机器）：放弃旧 blob，重新生成。
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成数据加密密钥失败: %w", err)
	}
	blob, err := platformProtect(key)
	if err != nil {
		return nil, fmt.Errorf("保护数据加密密钥失败: %w", err)
	}
	// 自检：确认平台保护可逆向，避免留下写坏的 blob 导致后续 lockout。
	if pt, err := platformUnprotect(blob); err != nil || string(pt) != string(key) {
		return nil, fmt.Errorf("数据加密密钥自检失败: %w", err)
	}
	if err := fsx.AtomicWrite(blobPath, blob, 0o600); err != nil {
		return nil, fmt.Errorf("保存数据加密密钥失败: %w", err)
	}
	return key, nil
}

// seal 用 AES-256-GCM 加密明文，12 字节随机 nonce 前缀在密文里。
func seal(plain, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("密钥长度非法")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

// open 解密 seal 产物；校验失败返回错误，不会 panic。
func open(ct, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("密钥长度非法")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ct) < gcm.NonceSize() {
		return nil, errors.New("密文过短")
	}
	nonce, data := ct[:gcm.NonceSize()], ct[gcm.NonceSize():]
	return gcm.Open(nil, nonce, data, nil)
}

// writeEncrypted 把凭据 JSON 加密后原子落盘。
func (s *Store) writeEncrypted(path string, cred *upstream.Credential) error {
	key, err := s.dek()
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	ct, err := seal(raw, key)
	if err != nil {
		return err
	}
	return fsx.AtomicWrite(path, append(ct, '\n'), 0o600)
}

// readCred 读取凭据：优先按加密格式解密；解密失败则回退到明文（升级前的遗留数据），
// 保证升级过程不会把旧账号「锁死」——下次 Save 会自动改写成加密格式。
func (s *Store) readCred(path string) (*upstream.Credential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw = dropTrailingLF(raw)
	if key, kerr := s.dek(); kerr == nil {
		if pt, oerr := open(raw, key); oerr == nil {
			var cred upstream.Credential
			if jerr := json.Unmarshal(pt, &cred); jerr == nil {
				return &cred, nil
			}
		}
	}
	// 回退：明文 JSON（升级前遗留）。
	var cred upstream.Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("凭据文件 %s 解析失败: %w", path, err)
	}
	return &cred, nil
}

func dropTrailingLF(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return b[:len(b)-1]
	}
	return b
}

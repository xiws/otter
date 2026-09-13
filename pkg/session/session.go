// Package session 提供会话的增删改查、活动会话管理以及本地持久化。
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Session 代表一个与 AI 平台的对话上下文。
type Session struct {
	ID           string            `json:"id"`       // 生成: "sess_" + 随机 id
	Provider     string            `json:"provider"` // "chatgpt" | "gemini" | "deepseek"
	Title        string            `json:"title"`    // 用户自定义标题或自动摘要
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	MessageCount int               `json:"message_count"`
	ExternalID   string            `json:"external_id,omitempty"` // 平台内部 ID
	Metadata     map[string]string `json:"metadata,omitempty"`    // Provider 特有上下文
}

// SessionManager 管理会话的持久化与生命周期。
type SessionManager struct {
	dataDir string
	mu      sync.RWMutex
}

// ActiveSessionEntry 记录当前活动的会话。
type ActiveSessionEntry struct {
	Provider  string `json:"provider"`
	SessionID string `json:"session_id"`
}

// NewSessionManager 创建一个新的会话管理器。
func NewSessionManager(dataDir string) *SessionManager {
	return &SessionManager{
		dataDir: dataDir,
	}
}

// generateID 生成一个随机会话 ID（sess_ 前缀 + 16 字节 hex）。
func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// fallback: 使用时间戳
		return fmt.Sprintf("sess_%x", time.Now().UnixNano())
	}
	return "sess_" + hex.EncodeToString(b)
}

// providerDir 返回 provider 的会话数据目录。
func (m *SessionManager) providerDir(provider string) string {
	return filepath.Join(m.dataDir, "sessions", provider)
}

// indexFile 返回 provider 的会话索引文件路径。
func (m *SessionManager) indexFile(provider string) string {
	return filepath.Join(m.providerDir(provider), "index.json")
}

// sessionFile 返回单个会话文件的路径。
func (m *SessionManager) sessionFile(sessionID string) string {
	// session ID 格式: sess_xxxxx，但我们需要知道 provider
	// 这里做一个简化：从 active_session 获取 provider，或者通过搜索
	// 实际上我们需要从 index 中查找
	return filepath.Join(m.dataDir, "sessions", sessionID+".json")
}

// sessionProviderFile 返回指定 provider 下某会话文件的路径。
func (m *SessionManager) sessionProviderFile(provider, sessionID string) string {
	return filepath.Join(m.providerDir(provider), sessionID+".json")
}

// activeFile 返回活动会话文件路径。
func (m *SessionManager) activeFile() string {
	return filepath.Join(m.dataDir, "active_session.json")
}

// Create 创建一个新会话。
func (m *SessionManager) Create(provider string, title string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if title == "" {
		title = fmt.Sprintf("%s 会话", provider)
	}

	sess := &Session{
		ID:        generateID(),
		Provider:  provider,
		Title:     title,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	// 写入会话文件
	if err := m.writeSession(sess); err != nil {
		return nil, err
	}

	// 更新索引
	if err := m.addToIndex(sess); err != nil {
		return nil, err
	}

	return sess, nil
}

// ActiveSession 返回当前 provider 的活动会话。
func (m *SessionManager) ActiveSession(provider string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, err := m.readActiveEntry()
	if err != nil {
		return nil, err
	}
	if entry == nil || entry.Provider != provider || entry.SessionID == "" {
		return nil, nil
	}

	return m.getByProvider(provider, entry.SessionID)
}

// SetActive 设置活动会话。
func (m *SessionManager) SetActive(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 先查找 session 的 provider
	sess, err := m.findSession(sessionID)
	if err != nil {
		return fmt.Errorf("会话 %s 不存在: %w", sessionID, err)
	}
	if sess == nil {
		return fmt.Errorf("会话 %s 不存在", sessionID)
	}

	entry := &ActiveSessionEntry{
		Provider:  sess.Provider,
		SessionID: sessionID,
	}
	return m.writeActiveEntry(entry)
}

// Get 根据 sessionID 获取会话。
func (m *SessionManager) Get(sessionID string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.findSession(sessionID)
}

// GetByID 根据 sessionID 从指定 provider 获取会话。
func (m *SessionManager) GetByID(provider, sessionID string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.getByProvider(provider, sessionID)
}

// List 列出指定 provider 的所有会话。
func (m *SessionManager) List(provider string) ([]*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.readIndex(provider)
}

// Delete 删除会话及其消息记录。
func (m *SessionManager) Delete(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, err := m.findSession(sessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("会话 %s 不存在", sessionID)
	}

	// 删除会话文件
	sessPath := m.sessionProviderFile(sess.Provider, sessionID)
	if err := os.Remove(sessPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除会话文件失败: %w", err)
	}

	// 从索引中移除
	if err := m.removeFromIndex(sess.Provider, sessionID); err != nil {
		return err
	}

	// 如果该会话是活动会话，清除活动状态
	active, _ := m.readActiveEntry()
	if active != nil && active.SessionID == sessionID {
		_ = os.Remove(m.activeFile())
	}

	return nil
}

// AddMessage 向会话添加消息。
func (m *SessionManager) AddMessage(sessionID string, msg *Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, err := m.findSession(sessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("会话 %s 不存在", sessionID)
	}

	// 读取当前消息列表
	msgs, err := m.readMessages(sess.Provider, sessionID)
	if err != nil {
		msgs = []*Message{}
	}

	msg.ID = generateID()
	msg.SessionID = sessionID
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	msgs = append(msgs, msg)

	// 写入消息
	if err := m.writeMessages(sess.Provider, sessionID, msgs); err != nil {
		return err
	}

	// 更新会话的 message_count 和 updated_at
	sess.MessageCount = len(msgs)
	sess.UpdatedAt = time.Now()

	// 重新保存会话元数据
	if err := m.writeSession(sess); err != nil {
		return err
	}
	// 索引里也存有会话元数据（session list 读的是索引），需要同步，
	// 否则列表里的消息数永远显示为 0。
	return m.updateIndex(sess)
}

// UpdateRemote 更新会话在平台侧的标识与上下文。
//
// externalID 为空时保持原值；metadata 中的键会合并进现有 metadata。
// 会话元数据同时写入会话文件与索引，保证后续读取到的是最新值。
func (m *SessionManager) UpdateRemote(sessionID, externalID string, metadata map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, err := m.findSession(sessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("会话 %s 不存在", sessionID)
	}

	if externalID != "" {
		sess.ExternalID = externalID
	}
	if len(metadata) > 0 {
		if sess.Metadata == nil {
			sess.Metadata = make(map[string]string, len(metadata))
		}
		for k, v := range metadata {
			sess.Metadata[k] = v
		}
	}
	sess.UpdatedAt = time.Now()

	if err := m.writeSession(sess); err != nil {
		return err
	}
	return m.updateIndex(sess)
}

// GetMessages 获取会话的消息列表。
func (m *SessionManager) GetMessages(sessionID string) ([]*Message, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sess, err := m.findSession(sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, fmt.Errorf("会话 %s 不存在", sessionID)
	}

	return m.readMessages(sess.Provider, sessionID)
}

// --- 内部方法 ---

// writeSession 将会话元数据写入 JSON 文件。
func (m *SessionManager) writeSession(sess *Session) error {
	dir := m.providerDir(sess.Provider)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建会话目录失败: %w", err)
	}

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化会话失败: %w", err)
	}

	path := filepath.Join(dir, sess.ID+".json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("写入会话文件失败: %w", err)
	}
	return nil
}

// readMessages 从磁盘读取消息列表。
func (m *SessionManager) readMessages(provider, sessionID string) ([]*Message, error) {
	path := filepath.Join(m.providerDir(provider), sessionID+"_msgs.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []*Message{}, nil
		}
		return nil, fmt.Errorf("读取消息文件失败: %w", err)
	}

	var msgs []*Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("解析消息文件失败: %w", err)
	}
	return msgs, nil
}

// writeMessages 将消息列表写入磁盘。
func (m *SessionManager) writeMessages(provider, sessionID string, msgs []*Message) error {
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化消息失败: %w", err)
	}

	path := filepath.Join(m.providerDir(provider), sessionID+"_msgs.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("写入消息文件失败: %w", err)
	}
	return nil
}

// readIndex 读取 provider 的会话索引。
func (m *SessionManager) readIndex(provider string) ([]*Session, error) {
	path := m.indexFile(provider)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []*Session{}, nil
		}
		return nil, fmt.Errorf("读取索引文件失败: %w", err)
	}

	var sessions []*Session
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, fmt.Errorf("解析索引文件失败: %w", err)
	}
	return sessions, nil
}

// writeIndex 写入 provider 的会话索引。
func (m *SessionManager) writeIndex(provider string, sessions []*Session) error {
	dir := m.providerDir(provider)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建会话目录失败: %w", err)
	}

	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化索引失败: %w", err)
	}

	path := m.indexFile(provider)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("写入索引文件失败: %w", err)
	}
	return nil
}

// addToIndex 将新会话添加到索引中。
func (m *SessionManager) addToIndex(sess *Session) error {
	sessions, err := m.readIndex(sess.Provider)
	if err != nil {
		return err
	}

	sessions = append(sessions, sess)
	return m.writeIndex(sess.Provider, sessions)
}

// updateIndex 用给定会话替换索引中的同 ID 记录；索引中不存在则追加。
func (m *SessionManager) updateIndex(sess *Session) error {
	sessions, err := m.readIndex(sess.Provider)
	if err != nil {
		return err
	}

	for i, s := range sessions {
		if s.ID == sess.ID {
			sessions[i] = sess
			return m.writeIndex(sess.Provider, sessions)
		}
	}
	sessions = append(sessions, sess)
	return m.writeIndex(sess.Provider, sessions)
}

// removeFromIndex 从索引中移除会话。
func (m *SessionManager) removeFromIndex(provider, sessionID string) error {
	sessions, err := m.readIndex(provider)
	if err != nil {
		return err
	}

	filtered := make([]*Session, 0, len(sessions))
	for _, s := range sessions {
		if s.ID != sessionID {
			filtered = append(filtered, s)
		}
	}
	return m.writeIndex(provider, filtered)
}

// findSession 在所有 provider 中搜索指定 ID 的会话。
func (m *SessionManager) findSession(sessionID string) (*Session, error) {
	providers := []string{"chatgpt", "gemini", "deepseek"}
	for _, p := range providers {
		sess, err := m.getByProvider(p, sessionID)
		if err != nil {
			return nil, err
		}
		if sess != nil {
			return sess, nil
		}
	}
	return nil, nil
}

// getByProvider 在指定 provider 中查找会话。
func (m *SessionManager) getByProvider(provider, sessionID string) (*Session, error) {
	sessions, err := m.readIndex(provider)
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if s.ID == sessionID {
			return s, nil
		}
	}
	return nil, nil
}

// readActiveEntry 读取活动会话记录。
func (m *SessionManager) readActiveEntry() (*ActiveSessionEntry, error) {
	path := m.activeFile()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取活动会话文件失败: %w", err)
	}

	var entry ActiveSessionEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("解析活动会话文件失败: %w", err)
	}
	return &entry, nil
}

// writeActiveEntry 写入活动会话记录。
func (m *SessionManager) writeActiveEntry(entry *ActiveSessionEntry) error {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化活动会话失败: %w", err)
	}

	if err := os.WriteFile(m.activeFile(), data, 0644); err != nil {
		return fmt.Errorf("写入活动会话文件失败: %w", err)
	}
	return nil
}

// ListAll 列出所有 provider 的所有会话。
func (m *SessionManager) ListAll() ([]*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	providers := []string{"chatgpt", "gemini", "deepseek"}
	var all []*Session
	for _, p := range providers {
		sessions, err := m.readIndex(p)
		if err != nil {
			return nil, err
		}
		all = append(all, sessions...)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].UpdatedAt.After(all[j].UpdatedAt)
	})
	return all, nil
}

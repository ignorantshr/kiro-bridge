package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var execCommand = exec.Command

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// Bridge manages the kiro-cli ACP process and provides prompt functionality.
type Bridge interface {
	Prompt(ctx context.Context, blocks []ContentBlock, onEvent func(PromptEvent)) (string, error)
	Models() []ModelInfo
	PromptCapabilities() PromptCapabilities
	Usage() UsageInfo
	Ready() bool
	Close() error
}

// PermissionDecider chooses which ACP permission option should be returned to
// the agent when a tool call requires explicit user confirmation. A nil
// decider means the bridge will always fall back to the default reject choice.
type PermissionDecider func(RequestPermissionParams) RequestPermissionOutcome

// UsageInfo holds the latest context usage snapshot reported by Kiro for the
// most recent turn that completed on this bridge.
type UsageInfo struct {
	ContextPercent float64
	TotalTokens    int
}

var contextWindow = parseContextWindow()

func parseContextWindow() int {
	if v := os.Getenv("KIRO_BRIDGE_CONTEXT_WINDOW"); v != "" {
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil && n > 0 {
			return n
		}
	}
	return 200000
}

// ModelInfo is the OpenAI-facing view of an ACP model entry returned by Kiro.
type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PromptEvent represents an event during a prompt turn.
type PromptEvent struct {
	Type       PromptEventType
	Text       string // for EventText
	ToolCallID string // for EventToolCall, EventToolCallUpdate
	ToolName   string // for EventToolCall
	ToolInput  string // for EventToolCall (JSON arguments)
	ToolStatus string // for EventToolCallUpdate
}

type PromptEventType int

const (
	EventText PromptEventType = iota
	EventToolCall
	EventToolCallUpdate
)

type SessionMode string

const (
	SessionModePerRequest SessionMode = "per_request"
	SessionModeShared     SessionMode = "shared"
)

func parseSessionMode() SessionMode {
	switch strings.ToLower(env("KIRO_BRIDGE_SESSION_MODE", string(SessionModePerRequest))) {
	case string(SessionModeShared):
		return SessionModeShared
	default:
		return SessionModePerRequest
	}
}

var errBridgeNotReady = errors.New("bridge not ready")

// bridge is the high-level runtime that supervises a single ACP process and
// exposes request-scoped session/turn execution to the HTTP layer.
type bridge struct {
	cfg         BridgeConfig
	sessionMode SessionMode

	promptMu sync.Mutex
	stateMu  sync.RWMutex
	nextID   int

	proc            *acpProcess
	models          []ModelInfo
	promptCaps      PromptCapabilities
	ready           bool
	sharedSessionID string
	lastContextPct  float64
	closed          bool

	stopCh chan struct{}
}

const maxConsecutiveErrors = 3
const cancelGraceTimeout = 500 * time.Millisecond

type BridgeConfig struct {
	CLIPath string
	CWD     string
	Agent   string
	Version string
	// PermissionDecider overrides the bridge's default permission response.
	// When nil, session/request_permission is answered with reject_once.
	PermissionDecider PermissionDecider
}

// acpSession is the bridge's internal handle for one ACP session plus the
// model snapshot observed when that session was created or loaded.
type acpSession struct {
	id     string
	models []ModelInfo
}

// acpSessionManager owns ACP session lifecycle operations such as new/load and
// applies any configured agent mode before a session is used for a turn.
type acpSessionManager struct {
	bridge  *bridge
	process *acpProcess
}

// turnRunner executes one session/prompt call, forwards turn events, and
// translates cancellation and terminal stop reasons back into bridge semantics.
type turnRunner struct {
	bridge  *bridge
	process *acpProcess
}

// acpProcess is the low-level JSON-RPC transport around a live `kiro-cli acp`
// child process. It is intentionally unaware of HTTP or session policy.
type acpProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader

	writeMu sync.Mutex

	// stateMu protects the live request router and the active notification sink.
	stateMu      sync.Mutex
	pending      map[RPCID]chan *Response
	notifHandler func(*Notification)
	deadErr      error
	permission   PermissionDecider
	done         chan struct{}
	deathOnce    sync.Once
	waitDone     chan struct{}
	waitErrMu    sync.RWMutex
	waitErr      error
}

func NewBridge(cfg BridgeConfig) (Bridge, error) {
	if cfg.PermissionDecider == nil {
		cfg.PermissionDecider = rejectPermissionDecider
	}
	b := &bridge{
		cfg:         cfg,
		sessionMode: parseSessionMode(),
		nextID:      1,
		stopCh:      make(chan struct{}),
	}
	if err := b.connect(); err != nil {
		b.Close()
		return nil, err
	}
	go b.supervise()
	return b, nil
}

func desiredPromptCapabilities() PromptCapabilities {
	return PromptCapabilities{
		Image:           true,
		Audio:           true,
		EmbeddedContext: true,
	}
}

// negotiatedPromptCapabilities intersects what the bridge knows how to emit
// with what the connected ACP agent declared during initialize.
func negotiatedPromptCapabilities(local, remote PromptCapabilities) PromptCapabilities {
	return PromptCapabilities{
		Image:           local.Image && remote.Image,
		Audio:           local.Audio && remote.Audio,
		EmbeddedContext: local.EmbeddedContext && remote.EmbeddedContext,
	}
}

func (b *bridge) connect() error {
	proc, err := newACPProcess(b.cfg.CLIPath, b.cfg.PermissionDecider)
	if err != nil {
		return fmt.Errorf("start kiro-cli: %w", err)
	}
	promptCaps, err := b.initialize(proc)
	if err != nil {
		proc.Close()
		return fmt.Errorf("initialize: %w", err)
	}

	manager := &acpSessionManager{bridge: b, process: proc}
	discoverySession, err := manager.NewSession()
	if err != nil {
		proc.Close()
		return fmt.Errorf("new session: %w", err)
	}

	sharedSessionID := ""
	if b.sessionMode == SessionModeShared {
		sharedSessionID = discoverySession.id
	}

	b.stateMu.Lock()
	if b.closed {
		b.stateMu.Unlock()
		proc.Close()
		return io.EOF
	}
	oldProc := b.proc
	b.proc = proc
	b.models = append([]ModelInfo(nil), discoverySession.models...)
	b.promptCaps = promptCaps
	b.ready = true
	b.sharedSessionID = sharedSessionID
	b.lastContextPct = 0
	b.stateMu.Unlock()

	if oldProc != nil && oldProc != proc {
		oldProc.Close()
	}

	log.Printf("bridge connected")
	return nil
}

func (b *bridge) supervise() {
	for {
		b.stateMu.RLock()
		closed := b.closed
		proc := b.proc
		b.stateMu.RUnlock()

		if closed {
			return
		}
		if proc == nil {
			if !b.reconnectLoop() {
				return
			}
			continue
		}

		// Wait for either an ACP process death or an explicit bridge shutdown.
		select {
		case <-proc.Done():
			b.handleProcessExit(proc)
		case <-b.stopCh:
			return
		}
	}
}

func (b *bridge) reconnectLoop() bool {
	delay := time.Second
	const maxDelay = 60 * time.Second

	for {
		select {
		case <-b.stopCh:
			return false
		default:
		}

		if err := b.connect(); err == nil {
			return true
		} else {
			log.Printf("failed to start bridge: %v (retrying in %s)", err, delay)
		}

		select {
		case <-b.stopCh:
			return false
		case <-time.After(delay):
		}

		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

func (b *bridge) handleProcessExit(proc *acpProcess) {
	b.stateMu.Lock()
	if b.proc != proc || b.closed {
		b.stateMu.Unlock()
		return
	}
	b.proc = nil
	b.promptCaps = PromptCapabilities{}
	b.ready = false
	b.sharedSessionID = ""
	b.lastContextPct = 0
	b.stateMu.Unlock()

	if err := proc.ExitErr(); err != nil {
		log.Printf("bridge process exited: %v", err)
	}
	go func() {
		_ = proc.Close()
	}()
}

func (b *bridge) initialize(proc *acpProcess) (PromptCapabilities, error) {
	localCaps := desiredPromptCapabilities()
	id := b.nextRequestID()
	resp, err := proc.sendRequest(id, "initialize", InitializeParams{
		ProtocolVersion: 1,
		ClientCapabilities: ClientCapabilities{
			PromptCapabilities: &localCaps,
		},
		ClientInfo: ClientInfo{Name: "kiro-bridge", Title: "Kiro Bridge", Version: b.cfg.Version},
	}, nil)
	if err != nil {
		return PromptCapabilities{}, fmt.Errorf("reading initialize response: %w", err)
	}
	if resp.Error != nil {
		return PromptCapabilities{}, fmt.Errorf("initialize error: %w", resp.Error)
	}
	var result InitializeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return PromptCapabilities{}, fmt.Errorf("decode initialize result: %w", err)
	}
	return negotiatedPromptCapabilities(localCaps, result.AgentCapabilities.PromptCapabilities), nil
}

func (b *bridge) nextRequestID() int {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	id := b.nextID
	b.nextID++
	return id
}

func (b *bridge) currentProcess() (*acpProcess, bool) {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()
	return b.proc, b.ready
}

func (b *bridge) Prompt(ctx context.Context, blocks []ContentBlock, onEvent func(PromptEvent)) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	b.promptMu.Lock()
	defer b.promptMu.Unlock()

	proc, sessionID, err := b.sessionForTurn()
	if err != nil {
		return "", err
	}

	runner := &turnRunner{bridge: b, process: proc}
	stopReason, contextPercent, err := runner.Run(ctx, sessionID, blocks, onEvent)
	if contextPercent >= 0 {
		b.stateMu.Lock()
		b.lastContextPct = contextPercent
		b.stateMu.Unlock()
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			b.handleProcessExit(proc)
			return "", errBridgeNotReady
		}
		select {
		case <-proc.Done():
			b.handleProcessExit(proc)
		default:
		}
		return "", err
	}
	return stopReason, nil
}

func (b *bridge) sessionForTurn() (*acpProcess, string, error) {
	proc, ready := b.currentProcess()
	if proc == nil || !ready {
		return nil, "", errBridgeNotReady
	}
	select {
	case <-proc.Done():
		b.handleProcessExit(proc)
		return nil, "", errBridgeNotReady
	default:
	}

	if b.sessionMode == SessionModeShared {
		b.stateMu.RLock()
		sharedSessionID := b.sharedSessionID
		b.stateMu.RUnlock()
		if sharedSessionID != "" {
			return proc, sharedSessionID, nil
		}
	}

	// In per-request mode we create a fresh ACP session for every HTTP request,
	// which avoids cross-request history bleed while still reusing the ACP process.
	manager := &acpSessionManager{bridge: b, process: proc}
	session, err := manager.NewSession()
	if err != nil {
		select {
		case <-proc.Done():
			b.handleProcessExit(proc)
			return nil, "", errBridgeNotReady
		default:
		}
		return nil, "", err
	}

	b.stateMu.Lock()
	b.models = append([]ModelInfo(nil), session.models...)
	if b.sessionMode == SessionModeShared {
		b.sharedSessionID = session.id
	}
	b.stateMu.Unlock()

	return proc, session.id, nil
}

func (b *bridge) Models() []ModelInfo {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()
	return append([]ModelInfo(nil), b.models...)
}

func (b *bridge) PromptCapabilities() PromptCapabilities {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()
	return b.promptCaps
}

func (b *bridge) Usage() UsageInfo {
	b.stateMu.RLock()
	pct := b.lastContextPct
	b.stateMu.RUnlock()
	tokens := int(pct / 100.0 * float64(contextWindow))
	return UsageInfo{ContextPercent: pct, TotalTokens: tokens}
}

func (b *bridge) Ready() bool {
	b.stateMu.RLock()
	proc := b.proc
	ready := b.ready
	b.stateMu.RUnlock()
	if !ready || proc == nil {
		return false
	}
	select {
	case <-proc.Done():
		b.handleProcessExit(proc)
		return false
	default:
		return true
	}
}

func (b *bridge) Close() error {
	b.stateMu.Lock()
	if b.closed {
		b.stateMu.Unlock()
		return nil
	}
	b.closed = true
	close(b.stopCh)
	proc := b.proc
	b.proc = nil
	b.promptCaps = PromptCapabilities{}
	b.ready = false
	b.sharedSessionID = ""
	b.stateMu.Unlock()

	if proc != nil {
		return proc.Close()
	}
	return nil
}

func newACPProcess(cliPath string, permission PermissionDecider) (*acpProcess, error) {
	cmd := execCommand(cliPath, "acp")
	cmd.Stderr = &stderrWriter{prefix: "[kiro-cli] "}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := &acpProcess{
		cmd:        cmd,
		stdin:      stdin,
		reader:     bufio.NewReader(stdout),
		pending:    make(map[RPCID]chan *Response),
		permission: permission,
		done:       make(chan struct{}),
		waitDone:   make(chan struct{}),
	}

	go proc.readLoop()

	go func() {
		err := cmd.Wait()
		proc.waitErrMu.Lock()
		proc.waitErr = err
		proc.waitErrMu.Unlock()
		close(proc.waitDone)
		proc.markDead(err)
	}()

	return proc, nil
}

func (p *acpProcess) Done() <-chan struct{} {
	return p.done
}

func (p *acpProcess) ExitErr() error {
	p.waitErrMu.RLock()
	defer p.waitErrMu.RUnlock()
	if p.waitErr != nil {
		return p.waitErr
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.deadErr
}

func (p *acpProcess) sendRequest(id int, method string, params any, onNotif func(*Notification)) (*Response, error) {
	data, err := newRequest(id, method, params)
	if err != nil {
		return nil, err
	}
	respCh, err := p.registerPending(IntID(id), onNotif)
	if err != nil {
		return nil, err
	}
	defer p.unregisterPending(IntID(id), respCh, onNotif)

	debugf("debug: acp >>> %s", data)
	if err := p.writeLine(data); err != nil {
		return nil, err
	}

	select {
	case resp, ok := <-respCh:
		if !ok || resp == nil {
			return nil, p.processError()
		}
		return resp, nil
	case <-p.done:
		return nil, p.processError()
	}
}

func (p *acpProcess) sendNotification(method string, params any) error {
	data, err := json.Marshal(Notification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  mustMarshal(params),
	})
	if err != nil {
		return err
	}
	debugf("debug: acp >>> %s", data)
	return p.writeLine(data)
}

func (p *acpProcess) writeLine(data []byte) error {
	select {
	case <-p.done:
		return p.processError()
	default:
	}

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	select {
	case <-p.done:
		return p.processError()
	default:
	}

	data = append(data, '\n')
	_, err := p.stdin.Write(data)
	if err != nil {
		p.markDead(err)
	}
	return err
}

func (p *acpProcess) readLoop() {
	for {
		line, err := p.reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimSpace(line)
			if len(line) > 0 {
				if handleErr := p.handleLine(line); handleErr != nil {
					p.markDead(handleErr)
					return
				}
			}
		}
		if err != nil {
			p.markDead(err)
			return
		}
	}
}

func (p *acpProcess) handleLine(line []byte) error {
	debugf("debug: acp <<< %s", line)

	switch classifyMessage(line) {
	case messageResponse:
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
		p.dispatchResponse(&resp)
		return nil

	case messageRequest:
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			return fmt.Errorf("parse request: %w", err)
		}
		p.handleIncomingRequest(&req)
		return nil

	case messageNotification:
		var notif Notification
		if err := json.Unmarshal(line, &notif); err != nil {
			return fmt.Errorf("parse notification: %w", err)
		}
		p.dispatchNotification(&notif)
		return nil

	default:
		return fmt.Errorf("unrecognized line from acp: %s", line)
	}
}

func (p *acpProcess) registerPending(id RPCID, onNotif func(*Notification)) (chan *Response, error) {
	select {
	case <-p.done:
		return nil, p.processError()
	default:
	}

	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.deadErr != nil {
		return nil, p.deadErr
	}
	if onNotif != nil && p.notifHandler != nil {
		return nil, fmt.Errorf("notification handler already registered")
	}
	respCh := make(chan *Response, 1)
	p.pending[id] = respCh
	if onNotif != nil {
		p.notifHandler = onNotif
	}
	return respCh, nil
}

func (p *acpProcess) unregisterPending(id RPCID, respCh chan *Response, onNotif func(*Notification)) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if current, ok := p.pending[id]; ok && current == respCh {
		delete(p.pending, id)
	}
	if onNotif != nil && p.notifHandler != nil {
		p.notifHandler = nil
	}
}

func (p *acpProcess) dispatchResponse(resp *Response) {
	p.stateMu.Lock()
	respCh, ok := p.pending[resp.ID]
	if ok {
		delete(p.pending, resp.ID)
	}
	p.stateMu.Unlock()
	if !ok {
		debugf("debug: ignoring unexpected response id=%+v", resp.ID)
		return
	}
	respCh <- resp
	close(respCh)
}

func (p *acpProcess) dispatchNotification(notif *Notification) {
	p.stateMu.Lock()
	handler := p.notifHandler
	p.stateMu.Unlock()
	if handler != nil {
		handler(notif)
	}
}

func (p *acpProcess) markDead(err error) {
	if err == nil {
		err = io.EOF
	}
	p.deathOnce.Do(func() {
		p.stateMu.Lock()
		p.deadErr = err
		pending := p.pending
		p.pending = make(map[RPCID]chan *Response)
		p.notifHandler = nil
		p.stateMu.Unlock()

		for _, ch := range pending {
			close(ch)
		}
		close(p.done)
	})
}

func (p *acpProcess) processError() error {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.deadErr != nil {
		return p.deadErr
	}
	return io.EOF
}

func (p *acpProcess) handleIncomingRequest(req *Request) {
	switch req.Method {
	case "session/request_permission":
		var params RequestPermissionParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			debugf("debug: failed to parse permission request: %v", err)
		}
		outcome := defaultPermissionOutcome(params.Options)
		if p.permission != nil {
			outcome = normalizePermissionOutcome(params.Options, p.permission(params))
		}
		result, err := json.Marshal(RequestPermissionResult{Outcome: outcome})
		if err != nil {
			debugf("debug: failed to marshal permission result: %v", err)
			return
		}
		data, err := newResponse(req.ID, result)
		if err != nil {
			debugf("debug: failed to marshal permission response: %v", err)
			return
		}
		_ = p.writeLine(data)
	default:
		data, err := newErrorResponse(req.ID, -32601, "Method not found")
		if err != nil {
			debugf("debug: failed to marshal error response: %v", err)
			return
		}
		_ = p.writeLine(data)
	}
}

func defaultPermissionOutcome(options []PermissionOption) RequestPermissionOutcome {
	for _, option := range options {
		if option.OptionID == "reject_once" {
			return RequestPermissionOutcome{Outcome: "selected", OptionID: option.OptionID}
		}
	}
	if len(options) > 0 {
		return RequestPermissionOutcome{Outcome: "selected", OptionID: options[0].OptionID}
	}
	return RequestPermissionOutcome{Outcome: "selected", OptionID: "reject_once"}
}

func normalizePermissionOutcome(options []PermissionOption, outcome RequestPermissionOutcome) RequestPermissionOutcome {
	if outcome.Outcome == "cancelled" {
		return RequestPermissionOutcome{Outcome: "cancelled"}
	}
	for _, option := range options {
		if option.OptionID == outcome.OptionID {
			return RequestPermissionOutcome{Outcome: "selected", OptionID: option.OptionID}
		}
	}
	return defaultPermissionOutcome(options)
}

// rejectPermissionDecider keeps the HTTP bridge deterministic: unless a caller
// explicitly injects a different policy, ACP permission requests are denied.
func rejectPermissionDecider(params RequestPermissionParams) RequestPermissionOutcome {
	return defaultPermissionOutcome(params.Options)
}

func (p *acpProcess) Close() error {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	select {
	case <-p.waitDone:
		return nil
	default:
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.waitDone:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.waitDone
	}
	return nil
}

func (p *acpProcess) ForceClose() error {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	select {
	case <-p.waitDone:
		return nil
	default:
	}
	_ = p.cmd.Process.Kill()
	<-p.waitDone
	return nil
}

func (m *acpSessionManager) NewSession() (*acpSession, error) {
	id := m.bridge.nextRequestID()
	resp, err := m.process.sendRequest(id, "session/new", NewSessionNewParams(m.bridge.cfg.CWD), nil)
	if err != nil {
		return nil, fmt.Errorf("reading session/new response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("session/new error: %w", resp.Error)
	}

	var result SessionNewResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, err
	}

	session := &acpSession{id: result.SessionID, models: modelsFromSession(result.Models)}
	if m.bridge.cfg.Agent != "" {
		if err := m.setMode(session.id); err != nil {
			return nil, fmt.Errorf("set mode: %w", err)
		}
	}
	return session, nil
}

func (m *acpSessionManager) LoadSession(sessionID string) (*acpSession, error) {
	id := m.bridge.nextRequestID()
	resp, err := m.process.sendRequest(id, "session/load", SessionLoadParams{SessionID: sessionID}, nil)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("session/load error: %w", resp.Error)
	}

	var result SessionLoadResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, err
	}

	session := &acpSession{id: result.SessionID, models: modelsFromSession(result.Models)}
	if m.bridge.cfg.Agent != "" {
		if err := m.setMode(session.id); err != nil {
			return nil, fmt.Errorf("set mode: %w", err)
		}
	}
	return session, nil
}

func (m *acpSessionManager) setMode(sessionID string) error {
	id := m.bridge.nextRequestID()
	resp, err := m.process.sendRequest(id, "session/set_mode", SessionSetModeParams{
		SessionID: sessionID,
		ModeID:    m.bridge.cfg.Agent,
	}, nil)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("set_mode error: %w", resp.Error)
	}
	return nil
}

func modelsFromSession(models *SessionModels) []ModelInfo {
	if models == nil {
		return nil
	}
	result := make([]ModelInfo, 0, len(models.AvailableModels))
	for _, model := range models.AvailableModels {
		result = append(result, ModelInfo{
			ID:          model.ModelID,
			Name:        model.Name,
			Description: model.Description,
		})
	}
	return result
}

func (r *turnRunner) Run(ctx context.Context, sessionID string, blocks []ContentBlock, onEvent func(PromptEvent)) (string, float64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", -1, err
	}
	if onEvent == nil {
		onEvent = func(PromptEvent) {}
	}

	var (
		cancelOnce     sync.Once
		turnStopReason string
		contextPercent = -1.0
		done           = make(chan struct{})
	)
	cancel := func() {
		cancelOnce.Do(func() {
			_ = r.process.sendNotification("session/cancel", SessionCancelParams{SessionID: sessionID})
		})
	}
	go func() {
		// HTTP cancellation is translated into ACP cancellation so a dead client
		// does not leave the shared process busy until the turn completes. If ACP
		// ignores cancellation, the process is force-closed so later requests can recover.
		select {
		case <-ctx.Done():
			cancel()
			timer := time.NewTimer(cancelGraceTimeout)
			defer timer.Stop()
			select {
			case <-done:
			case <-r.process.Done():
			case <-timer.C:
				_ = r.process.ForceClose()
			}
		case <-done:
		}
	}()

	id := r.bridge.nextRequestID()
	resp, err := r.process.sendRequest(id, "session/prompt", SessionPromptParams{
		SessionID: sessionID,
		Content:   blocks,
	}, func(notif *Notification) {
		if notif.Method == "_kiro.dev/metadata" {
			var meta struct {
				ContextUsagePercentage float64 `json:"contextUsagePercentage"`
			}
			if err := json.Unmarshal(notif.Params, &meta); err == nil {
				contextPercent = meta.ContextUsagePercentage
			}
			return
		}
		if notif.Method != "session/update" && notif.Method != "session/notification" {
			return
		}

		var params SessionUpdateParams
		if err := json.Unmarshal(notif.Params, &params); err != nil {
			debugf("debug: unmarshal session update params: %v", err)
			return
		}

		var update SessionUpdate
		if err := json.Unmarshal(params.Update, &update); err != nil {
			debugf("debug: unmarshal session update: %v", err)
			return
		}

		switch normalizeACPName(update.SessionUpdate) {
		case "agent_message_chunk":
			if text := update.ContentText(); text != "" {
				onEvent(PromptEvent{Type: EventText, Text: text})
			}
		case "tool_call":
			name := update.Title
			var raw struct {
				Meta *struct {
					ToolName string `json:"tool_name"`
				} `json:"_meta"`
				RawInput json.RawMessage `json:"rawInput"`
			}
			_ = json.Unmarshal(params.Update, &raw)
			if raw.Meta != nil && raw.Meta.ToolName != "" {
				name = raw.Meta.ToolName
			}
			ev := PromptEvent{
				Type:       EventToolCall,
				ToolCallID: update.ToolCallID,
				ToolName:   name,
				ToolStatus: update.Status,
			}
			if raw.RawInput != nil {
				ev.ToolInput = string(raw.RawInput)
			}
			onEvent(ev)
		case "tool_call_update":
			onEvent(PromptEvent{
				Type:       EventToolCallUpdate,
				ToolCallID: update.ToolCallID,
				ToolStatus: update.Status,
			})
		case "turn_end":
			// Prefer TurnEnd when present because newer ACP flows report the
			// terminal stop reason there instead of only in the prompt response.
			turnStopReason = update.StopReason
		}
	})
	close(done)

	if err != nil {
		if ctx.Err() != nil {
			return "", contextPercent, ctx.Err()
		}
		return "", contextPercent, err
	}
	if resp.Error != nil {
		if ctx.Err() != nil {
			return "", contextPercent, ctx.Err()
		}
		return "", contextPercent, fmt.Errorf("prompt error: %w", resp.Error)
	}
	if turnStopReason != "" {
		return turnStopReason, contextPercent, nil
	}

	var result SessionPromptResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", contextPercent, nil
	}
	return result.StopReason, contextPercent, nil
}

func normalizeACPName(name string) string {
	if name == "" {
		return ""
	}
	var b strings.Builder
	// Normalize both camel case and snake case ACP event names into one internal form
	// so the rest of the bridge can stay version-agnostic.
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteByte(byte(r - 'A' + 'a'))
		case r == '-' || r == ' ':
			b.WriteByte('_')
		default:
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

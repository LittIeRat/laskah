// Command devshot 是开发期截图工具：登录 Laskah 后用 Chrome DevTools Protocol 截取页面。
//
// 只用于本地预览验证，不参与服务端运行，也不会被主二进制引用。
//
//	go run ./tools/devshot -base http://127.0.0.1:8787 -user "Digital Gleam" -password "..." -out _preview
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	base := flag.String("base", "http://127.0.0.1:8787", "Laskah 站点地址")
	user := flag.String("user", "", "管理员账户（留空则只截公开页）")
	password := flag.String("password", "", "管理员密码")
	devtools := flag.String("devtools", "http://127.0.0.1:9222", "Chrome 远程调试地址")
	out := flag.String("out", "_preview", "截图输出目录")
	theme := flag.String("theme", "", "预设主题：light / dark / auto，留空则不干预")
	pages := flag.String("pages", "", "要截取的路径，逗号分隔；留空按登录态自动选择")
	eval := flag.String("eval", "", "截图前在页面里执行的 JS（用于展开弹窗等交互态）")
	suffix := flag.String("suffix", "", "输出文件名附加后缀")
	width := flag.Int("width", 1440, "视口宽度")
	height := flag.Int("height", 960, "视口高度")
	flag.Parse()

	if err := run(*base, *user, *password, *devtools, *out, *theme, *pages, *eval, *suffix, *width, *height); err != nil {
		fmt.Fprintln(os.Stderr, "devshot 失败:", err)
		os.Exit(1)
	}
}

func run(base, user, password, devtools, out, theme, pages, eval, suffix string, width, height int) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	cookie := ""
	if user != "" {
		token, err := login(base, user, password)
		if err != nil {
			return fmt.Errorf("登录失败: %w", err)
		}
		cookie = token
	}

	targets := []string{}
	if trimmed := strings.TrimSpace(pages); trimmed != "" {
		for _, item := range strings.Split(trimmed, ",") {
			if cleaned := strings.TrimSpace(item); cleaned != "" {
				targets = append(targets, cleaned)
			}
		}
	} else if cookie != "" {
		targets = []string{"/dashboard", "/manage"}
	} else {
		targets = []string{"/setup", "/login"}
	}

	wsURL, err := newTarget(devtools)
	if err != nil {
		return fmt.Errorf("连接 Chrome 调试端口失败（请先用 -remote-debugging-port=9222 启动 Chrome）: %w", err)
	}
	client, err := dial(wsURL)
	if err != nil {
		return err
	}
	defer client.Close()

	if _, err := client.Call("Page.enable", nil); err != nil {
		return err
	}
	if _, err := client.Call("Network.enable", nil); err != nil {
		return err
	}
	if _, err := client.Call("Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             width,
		"height":            height,
		"deviceScaleFactor": 2,
		"mobile":            false,
	}); err != nil {
		return err
	}

	host, err := hostOf(base)
	if err != nil {
		return err
	}
	if cookie != "" {
		if _, err := client.Call("Network.setCookie", map[string]any{
			"name":     "laskah_session",
			"value":    cookie,
			"domain":   host,
			"path":     "/",
			"httpOnly": true,
		}); err != nil {
			return err
		}
	}
	if theme != "" {
		// localStorage 只能在页面上下文里写，先访问同源页面再注入。
		if _, err := client.Call("Page.navigate", map[string]any{"url": base + "/login"}); err != nil {
			return err
		}
		client.WaitLoad(6 * time.Second)
		script := fmt.Sprintf("localStorage.setItem('laskah.theme','%s')", theme)
		if _, err := client.Call("Runtime.evaluate", map[string]any{"expression": script}); err != nil {
			return err
		}
	}

	for _, page := range targets {
		target := base + page
		if _, err := client.Call("Page.navigate", map[string]any{"url": target}); err != nil {
			return err
		}
		client.WaitLoad(10 * time.Second)
		// 等前端拉完数据再截图。
		time.Sleep(1400 * time.Millisecond)

		if eval != "" {
			if _, err := client.Call("Runtime.evaluate", map[string]any{"expression": eval, "awaitPromise": true}); err != nil {
				return err
			}
			time.Sleep(900 * time.Millisecond)
		}

		result, err := client.Call("Page.captureScreenshot", map[string]any{
			"format":                "png",
			"captureBeyondViewport": true,
		})
		if err != nil {
			return err
		}
		payload := struct {
			Data string `json:"data"`
		}{}
		if err := json.Unmarshal(result, &payload); err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(payload.Data)
		if err != nil {
			return err
		}
		name := strings.Trim(strings.ReplaceAll(strings.TrimPrefix(page, "/"), "/", "-"), "-")
		if name == "" {
			name = "index"
		}
		if theme != "" {
			name += "-" + theme
		}
		name += suffix
		file := filepath.Join(out, name+".png")
		if err := os.WriteFile(file, raw, 0o644); err != nil {
			return err
		}
		fmt.Printf("已保存 %s (%d 字节)\n", file, len(raw))
	}
	return nil
}

// login 用管理面接口换取会话 Cookie。
func login(base, user, password string) (string, error) {
	payload, err := json.Marshal(map[string]string{"user": user, "password": password})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequest(http.MethodPost, base+"/admin/login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	for _, item := range response.Cookies() {
		if item.Name == "laskah_session" {
			return item.Value, nil
		}
	}
	return "", errors.New("响应未包含会话 Cookie")
}

func hostOf(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	host := parsed.Hostname()
	if host == "" {
		return "", errors.New("无法解析主机名: " + base)
	}
	return host, nil
}

// newTarget 打开一个空白标签页并返回其 WebSocket 调试地址。
func newTarget(devtools string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	request, err := http.NewRequest(http.MethodPut, strings.TrimRight(devtools, "/")+"/json/new?about:blank", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	target := struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}{}
	if err := json.Unmarshal(body, &target); err != nil {
		return "", err
	}
	if target.WebSocketDebuggerURL == "" {
		return "", errors.New("调试响应缺少 webSocketDebuggerUrl")
	}
	return target.WebSocketDebuggerURL, nil
}

// ---------- 极简 WebSocket 客户端（仅够跑 CDP，不做扩展协商与压缩） ----------

type conn struct {
	socket net.Conn
	reader *bufReader
	nextID int
}

type bufReader struct {
	socket net.Conn
	buffer []byte
}

func (b *bufReader) readFull(n int) ([]byte, error) {
	for len(b.buffer) < n {
		chunk := make([]byte, 32*1024)
		count, err := b.socket.Read(chunk)
		if count > 0 {
			b.buffer = append(b.buffer, chunk[:count]...)
		}
		if err != nil {
			return nil, err
		}
	}
	out := b.buffer[:n]
	b.buffer = b.buffer[n:]
	return out, nil
}

func dial(rawURL string) (*conn, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := parsed.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	socket, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return nil, err
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		socket.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	path := parsed.Path
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	handshake := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + parsed.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := socket.Write([]byte(handshake)); err != nil {
		socket.Close()
		return nil, err
	}

	reader := &bufReader{socket: socket}
	header := []byte{}
	for !bytes.Contains(header, []byte("\r\n\r\n")) {
		chunk, err := reader.readFull(1)
		if err != nil {
			socket.Close()
			return nil, err
		}
		header = append(header, chunk...)
		if len(header) > 8192 {
			socket.Close()
			return nil, errors.New("握手响应过大")
		}
	}
	if !bytes.Contains(header, []byte("101")) {
		socket.Close()
		return nil, errors.New("WebSocket 握手被拒绝: " + strings.SplitN(string(header), "\r\n", 2)[0])
	}
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	expected := base64.StdEncoding.EncodeToString(digest[:])
	if !strings.Contains(string(header), expected) {
		socket.Close()
		return nil, errors.New("Sec-WebSocket-Accept 校验失败")
	}
	return &conn{socket: socket, reader: reader, nextID: 1}, nil
}

func (c *conn) Close() {
	_ = c.socket.Close()
}

func (c *conn) writeFrame(opcode byte, payload []byte) error {
	header := []byte{0x80 | opcode}
	size := len(payload)
	switch {
	case size < 126:
		header = append(header, byte(0x80|size))
	case size < 65536:
		header = append(header, 0x80|126)
		var extended [2]byte
		binary.BigEndian.PutUint16(extended[:], uint16(size))
		header = append(header, extended[:]...)
	default:
		header = append(header, 0x80|127)
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], uint64(size))
		header = append(header, extended[:]...)
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header = append(header, mask...)
	masked := make([]byte, size)
	for i := 0; i < size; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.socket.Write(append(header, masked...)); err != nil {
		return err
	}
	return nil
}

// readMessage 读取一条完整消息，自动拼接分片并回应 ping。
func (c *conn) readMessage() ([]byte, error) {
	message := []byte{}
	for {
		head, err := c.reader.readFull(2)
		if err != nil {
			return nil, err
		}
		fin := head[0]&0x80 != 0
		opcode := head[0] & 0x0f
		size := int(head[1] & 0x7f)
		switch size {
		case 126:
			extended, err := c.reader.readFull(2)
			if err != nil {
				return nil, err
			}
			size = int(binary.BigEndian.Uint16(extended))
		case 127:
			extended, err := c.reader.readFull(8)
			if err != nil {
				return nil, err
			}
			size = int(binary.BigEndian.Uint64(extended))
		}
		payload, err := c.reader.readFull(size)
		if err != nil {
			return nil, err
		}
		copied := make([]byte, len(payload))
		copy(copied, payload)

		switch opcode {
		case 0x9:
			if err := c.writeFrame(0xA, copied); err != nil {
				return nil, err
			}
			continue
		case 0xA:
			continue
		case 0x8:
			return nil, errors.New("连接已被对端关闭")
		}
		message = append(message, copied...)
		if fin {
			return message, nil
		}
	}
}

// Call 发送一条 CDP 命令并返回 result。
func (c *conn) Call(method string, params map[string]any) ([]byte, error) {
	id := c.nextID
	c.nextID++
	if params == nil {
		params = map[string]any{}
	}
	request, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err := c.writeFrame(0x1, request); err != nil {
		return nil, err
	}
	_ = c.socket.SetReadDeadline(time.Now().Add(60 * time.Second))
	for {
		raw, err := c.readMessage()
		if err != nil {
			return nil, err
		}
		envelope := struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}{}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			continue
		}
		if envelope.ID != id {
			continue
		}
		if envelope.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, envelope.Error.Message)
		}
		return envelope.Result, nil
	}
}

// WaitLoad 等待 Page.loadEventFired，超时即返回。
func (c *conn) WaitLoad(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = c.socket.SetReadDeadline(deadline)
		raw, err := c.readMessage()
		if err != nil {
			return
		}
		event := struct {
			Method string `json:"method"`
		}{}
		if err := json.Unmarshal(raw, &event); err != nil {
			continue
		}
		if event.Method == "Page.loadEventFired" {
			return
		}
	}
}

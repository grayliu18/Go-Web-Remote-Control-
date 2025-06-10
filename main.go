package main

import (
	"context" // Added for server shutdown
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort" // Added for sorting IPs
	"strings"
	"sync" // Added for shutdownOnce
	"time"

	"github.com/gen2brain/beeep"
	"github.com/getlantern/systray"              // Added systray
	registry "golang.org/x/sys/windows/registry" // Added registry import with alias

	// Removed: "github.com/getlantern/systray/example/icon"
	_ "embed" // Added for go:embed

	// Added for startup functionality

	"github.com/go-vgo/robotgo"
	"github.com/gorilla/websocket"
)

//go:embed ico.ico
var iconData []byte

//go:embed index.html
var indexHTMLContent []byte

// Global variables for server instance, shutdown control, and selected IP
var (
	server       *http.Server
	shutdownOnce sync.Once
	selectedIP   string
	ipMutex      sync.RWMutex // Mutex for selectedIP
)

const (
	startupAppName = "GoRemoteControl" // Name for registry key
	defaultPort    = 61336
	// htmlFilePath     = "index.html" // Removed: No longer needed as HTML is embedded
	selfCheckWait    = 3 * time.Second
	selfCheckTimeout = 5 * time.Second
	notifyTimeoutMs  = 5000 // beeep uses milliseconds
)

// WebSocket Upgrader
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Allow all origins for simplicity in local network
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Command structure for incoming WebSocket messages
// {{ modifications }}
type Command struct {
	Type      string   `json:"type"`                // 指令类型
	Dx        int      `json:"dx,omitempty"`        // 鼠标水平移动距离
	Dy        int      `json:"dy,omitempty"`        // 鼠标垂直移动距离
	Amount    int      `json:"amount,omitempty"`    // 滚动量
	Button    string   `json:"button,omitempty"`    // 鼠标按键 (left, right, middle)
	Value     string   `json:"value,omitempty"`     // 按键值 (e.g., "a", "enter", "ctrl")
	Text      string   `json:"text,omitempty"`      // 要输入的文本
	Modifiers []string `json:"modifiers,omitempty"` // 新增：修饰键列表 (e.g., ["ctrl", "shift"])
}

// serveHome handles requests for the root path, serving the embedded index.html
func serveHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// Set content type to HTML
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Write the embedded HTML content to the response
	_, err := w.Write(indexHTMLContent)
	if err != nil {
		// Log error if writing response fails
		log.Printf("写入嵌入的 HTML 响应时出错: %v", err)
		http.Error(w, "无法提供页面", http.StatusInternalServerError)
	}
}

// handleConnections upgrades HTTP requests to WebSocket connections
func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket 升级失败: %v", err)
		return
	}
	defer ws.Close() // Ensure connection is closed when function returns

	clientAddr := ws.RemoteAddr().String()
	log.Printf("客户端已连接: %s", clientAddr)

	// Send connection notification
	notify("网页键鼠", fmt.Sprintf("客户端已连接: %s", clientAddr), "")

	// Handle incoming messages in a separate loop for this connection
	handleMessages(ws)
}

// handleMessages reads and processes messages from a WebSocket client
func handleMessages(ws *websocket.Conn) {
	clientAddr := ws.RemoteAddr().String()
	defer func() {
		// Clean up mouse state on disconnect
		robotgo.MouseUp("left")
		robotgo.MouseUp("right")
		// Clean up modifier keys state on disconnect (important!)
		// robotgo.KeyToggle("shift", "up") // Example, robotgo doesn't directly track state like this
		// robotgo.KeyToggle("ctrl", "up")
		// robotgo.KeyToggle("alt", "up")
		// robotgo.KeyToggle("cmd", "up") // For mac
		// It's generally safer to rely on the OS handling modifier key releases
		// when the application exits or connection drops. Explicitly sending 'up'
		// might interfere if the user is physically holding the key.
		log.Printf("清理并关闭与 %s 的连接。", clientAddr)
	}()

	for {
		// Read message from browser
		_, msgBytes, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket 错误 (客户端: %s): %v", clientAddr, err)
			} else {
				log.Printf("客户端断开连接: %s", clientAddr)
			}
			break // Exit loop on error or disconnect
		}

		// Parse the JSON message
		var cmd Command
		if err := json.Unmarshal(msgBytes, &cmd); err != nil {
			log.Printf("收到非JSON格式消息来自 %s: %s, 错误: %v", clientAddr, string(msgBytes), err)
			continue
		}

		// Execute the command
		// log.Printf("收到指令: %+v", cmd) // Debug: Print received command
		err = executeCommand(cmd)
		if err != nil {
			log.Printf("执行命令失败 (来自 %s): %v - 命令: %+v", clientAddr, err, cmd)
		}
	}
}

// executeCommand performs the action based on the command type
func executeCommand(cmd Command) error {
	switch cmd.Type {
	case "move":
		if cmd.Dx != 0 || cmd.Dy != 0 {
			robotgo.MoveRelative(cmd.Dx, cmd.Dy)
		}
	case "click":
		robotgo.Click("left", false) // false = not double click
	case "double_click":
		robotgo.Click("left", true) // true = double click
	case "right_click":
		robotgo.Click("right", false)
	case "scroll":
		if cmd.Amount != 0 {
			robotgo.Scroll(0, -cmd.Amount)
		}
	case "mouse_down":
		// Frontend only sends 'left' currently, but support others if needed
		button := strings.ToLower(cmd.Button)
		if button == "" {
			button = "left" // Default to left if not specified
		}
		robotgo.Toggle(button, "down")
	case "mouse_up":
		button := strings.ToLower(cmd.Button)
		if button == "" {
			button = "left" // Default to left if not specified
		}
		robotgo.Toggle(button, "up")
	case "key_press":
		// {{ modifications }}
		key := translateKey(cmd.Value)
		if key != "" {
			// Check for modifiers
			if len(cmd.Modifiers) > 0 {
				// Ensure modifiers are also translated if necessary (though ctrl, alt, shift are usually standard)
				validModifiers := []string{}
				for _, mod := range cmd.Modifiers {
					translatedMod := translateKey(mod) // Translate modifier keys too
					if translatedMod != "" {
						validModifiers = append(validModifiers, translatedMod)
					} else {
						log.Printf("警告：收到未知的修饰键 '%s'，已忽略。", mod)
					}
				}
				if len(validModifiers) > 0 {
					log.Printf("执行 KeyTap: key='%s', modifiers=%v", key, validModifiers) // Debug
					robotgo.KeyTap(key, validModifiers)                                  // Use KeyTap with modifiers
				} else {
					log.Printf("执行 KeyTap (无有效修饰键): key='%s'", key) // Debug
					robotgo.KeyTap(key)                             // Fallback if all modifiers were invalid
				}
			} else {
				// No modifiers sent
				log.Printf("执行 KeyTap (无修饰键): key='%s'", key) // Debug
				robotgo.KeyTap(key)
			}
		} else {
			log.Printf("未知的按键值: %s", cmd.Value)
		}
	case "typewrite":
		if cmd.Text != "" {
			robotgo.TypeStr(cmd.Text)
		}
	case "shutdown":
		log.Println("接收到关机指令，准备执行...")
		go executeShutdown() // Run in goroutine to avoid blocking websocket handler? Maybe not necessary.
	default:
		return fmt.Errorf("未知的指令类型: %s", cmd.Type)
	}
	return nil
}

// translateKey converts frontend key names to robotgo key names
func translateKey(keyName string) string {
	// Map common keys, refer to robotgo documentation for specific names
	// https://github.com/go-vgo/robotgo/blob/master/docs/keys.md
	// Ensure map keys are lowercase for case-insensitive matching
	keyMap := map[string]string{
		"escape":    "esc",
		"tab":       "tab",
		"backspace": "backspace",
		"enter":     "enter",
		"up":        "up",
		"down":      "down",
		"left":      "left",
		"right":     "right",
		"space":     "space",
		"shift":     "shift", // Modifier key
		"ctrl":      "ctrl",  // Modifier key
		"alt":       "alt",   // Modifier key
		"win":       "cmd",   // Map 'win' to 'cmd' (robotgo uses 'cmd' for Win/Command)
		"cmd":       "cmd",   // Allow 'cmd' directly too
		"capslock":  "capslock",
		"f1":        "f1", "f2": "f2", "f3": "f3", "f4": "f4", "f5": "f5", "f6": "f6",
		"f7": "f7", "f8": "f8", "f9": "f9", "f10": "f10", "f11": "f11", "f12": "f12",
		// Add other keys as needed from index.html
		"`": "`", "1": "1", "2": "2", "3": "3", "4": "4", "5": "5", "6": "6", "7": "7", "8": "8", "9": "9", "0": "0", "-": "-", "=": "=",
		"q": "q", "w": "w", "e": "e", "r": "r", "t": "t", "y": "y", "u": "u", "i": "i", "o": "o", "p": "p", "[": "[", "]": "]", "\\": "\\",
		"a": "a", "s": "s", "d": "d", "f": "f", "g": "g", "h": "h", "j": "j", "k": "k", "l": "l", ";": ";", "'": "'",
		"z": "z", "x": "x", "c": "c", "v": "v", "b": "b", "n": "n", "m": "m", ",": ",", ".": ".", "/": "/",
		"delete": "delete", // Added delete key mapping
		// Keys that might not be needed for KeyTap but good to have?
		// "pageup": "pageup", "pagedown": "pagedown", "home": "home", "end": "end", "insert": "insert",
	}
	lowerKeyName := strings.ToLower(keyName)
	if translated, ok := keyMap[lowerKeyName]; ok {
		return translated
	}
	// If it's a single character and not in the map, assume it's a direct key tap
	if len(keyName) == 1 {
		// Should we return lowercase here? robotgo might handle case. Let's keep original case for TypeStr if needed later.
		// For KeyTap, case might not matter for letters if modifiers handle shift.
		return keyName // Return original case for single chars not in map
	}
	log.Printf("警告: translateKey 无法翻译 '%s'", keyName)
	return "" // Return empty if key is unknown
}

// executeShutdown performs the system shutdown command based on OS
func executeShutdown() {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("shutdown", "/s", "/t", "0")
	case "linux":
		// Requires root privileges usually
		cmd = exec.Command("shutdown", "-h", "now")
		log.Println("警告: Linux 关机通常需要 root 权限。")
	case "darwin": // macOS
		// Requires root privileges usually
		cmd = exec.Command("shutdown", "-h", "now")
		log.Println("警告: macOS 关机通常需要 root 权限。")
	default:
		log.Printf("不支持的操作系统用于关机: %s", runtime.GOOS)
		notify("关机错误", fmt.Sprintf("不支持的操作系统: %s", runtime.GOOS), "")
		return
	}

	log.Printf("正在执行关机命令: %s", cmd.String())
	notify("正在关机", "电脑将在几秒钟内关闭...", "")

	err := cmd.Run()
	if err != nil {
		log.Printf("执行关机命令失败: %v", err)
		notify("关机失败", fmt.Sprintf("错误: %v", err), "")
	}
	// If successful, the program might terminate before logging further.
}

// --- 开机自启相关函数 (仅 Windows) ---

// getExecutablePath 获取当前运行程序的可执行文件完整路径
func getExecutablePath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("获取可执行文件路径失败: %v", err)
		return "", fmt.Errorf("获取可执行文件路径失败: %w", err)
	}
	// 确保路径是绝对路径 (os.Executable 通常返回绝对路径，但以防万一)
	// 注意：在 Windows 上，os.Executable 返回的路径可能包含反斜杠 '\'
	return exePath, nil
}

// isStartupEnabled 检查指定的应用程序是否已设置为开机自启 (仅 Windows)
func isStartupEnabled(appName string, executablePath string) (bool, error) {
	if runtime.GOOS != "windows" {
		return false, fmt.Errorf("开机自启检查仅支持 Windows")
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		// 如果 Run 键不存在，也视为未启用
		if err == registry.ErrNotExist {
			return false, nil
		}
		log.Printf("打开注册表 Run 键失败: %v", err)
		return false, fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer key.Close()

	val, _, err := key.GetStringValue(appName)
	if err != nil {
		// 如果值不存在，视为未启用
		if err == registry.ErrNotExist {
			return false, nil
		}
		log.Printf("读取注册表值 %s 失败: %v", appName, err)
		return false, fmt.Errorf("读取注册表值 %s 失败: %w", appName, err)
	}

	// 比较路径是否一致 (忽略大小写和斜杠方向可能带来的差异，虽然 os.Executable 应该是一致的)
	// 简单的字符串比较通常足够，因为我们是用 os.Executable 获取的路径写入的
	return strings.EqualFold(val, executablePath), nil
}

// enableStartup 将指定的应用程序添加到开机自启 (仅 Windows)
func enableStartup(appName string, executablePath string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("启用开机自启仅支持 Windows")
	}
	// 注意：路径中可能包含空格，需要用引号括起来，以便命令行正确解析
	// registry 包会自动处理，但如果手动构造命令行则需要注意
	// quotedPath := fmt.Sprintf(`"%s"`, executablePath) // 移除：未使用

	key, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		log.Printf("创建/打开注册表 Run 键失败: %v", err)
		return fmt.Errorf("创建/打开注册表 Run 键失败: %w", err)
	}
	defer key.Close()

	err = key.SetStringValue(appName, executablePath) // 直接写入原始路径
	if err != nil {
		log.Printf("写入注册表值 %s 失败: %v", appName, err)
		return fmt.Errorf("写入注册表值 %s 失败: %w", appName, err)
	}
	log.Printf("已成功启用开机自启: %s -> %s", appName, executablePath)
	return nil
}

// disableStartup 从开机自启中移除指定的应用程序 (仅 Windows)
func disableStartup(appName string, _ string) error { // executablePath (占位符 _) 暂时未使用，但保留以备将来验证
	if runtime.GOOS != "windows" {
		return fmt.Errorf("禁用开机自启仅支持 Windows")
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE) // 需要写权限来删除
	if err != nil {
		// 如果键或值不存在，也视为已禁用或操作成功
		if err == registry.ErrNotExist {
			log.Printf("注册表 Run 键不存在，无需禁用。")
			return nil
		}
		log.Printf("打开注册表 Run 键失败 (用于删除): %v", err)
		return fmt.Errorf("打开注册表 Run 键失败 (用于删除): %w", err)
	}
	defer key.Close()

	// 检查值是否存在，避免删除不存在的值时报错 (虽然 DeleteValue 不存在时通常也返回 ErrNotExist)
	_, _, err = key.GetStringValue(appName)
	if err == registry.ErrNotExist {
		log.Printf("注册表值 %s 不存在，无需禁用。", appName)
		return nil // 值不存在，视为禁用成功
	}

	err = key.DeleteValue(appName)
	if err != nil && err != registry.ErrNotExist { // 再次检查 ErrNotExist 以防万一
		log.Printf("删除注册表值 %s 失败: %v", appName, err)
		return fmt.Errorf("删除注册表值 %s 失败: %w", appName, err)
	}
	log.Printf("已成功禁用开机自启: %s", appName)
	return nil
}

// --- End Startup Functions ---

// IPInfo holds IP address and its associated interface name
type IPInfo struct {
	IP            string
	InterfaceName string
}

// getAllLocalIPs finds all suitable non-loopback private IPv4 addresses.
// It returns a list of IPInfo structs, sorted with 192.168.x.x addresses first.
func getAllLocalIPs() []IPInfo {
	var ips []IPInfo
	interfaces, err := net.Interfaces()
	if err != nil {
		log.Printf("获取网络接口失败: %v", err)
		return ips // Return empty list
	}

	for _, i := range interfaces {
		// Basic check to skip potentially non-physical or down interfaces
		// Also skip known virtual/tunneling interface names
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		lowerName := strings.ToLower(i.Name)
		if strings.Contains(lowerName, "virtual") || strings.Contains(lowerName, "docker") || strings.Contains(lowerName, "vmnet") || strings.Contains(lowerName, "loopback") || strings.Contains(lowerName, "teredo") || strings.Contains(lowerName, "isatap") || strings.Contains(lowerName, "bluetooth") {
			continue
		}

		addrs, err := i.Addrs()
		if err != nil {
			log.Printf("获取接口 %s 的地址失败: %v", i.Name, err)
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4() // Ensure it's IPv4
			if ip == nil {
				continue // Not an IPv4 address
			}

			// Check if it's a private IP range
			isPrivate := ip[0] == 192 && ip[1] == 168 ||
				ip[0] == 10 ||
				(ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31)

			if isPrivate {
				log.Printf("发现可用 IP: %s (接口: %s)", ip.String(), i.Name)
				ips = append(ips, IPInfo{IP: ip.String(), InterfaceName: i.Name})
			}
		}
	}

	// Sort IPs: 192.168.x.x first, then others alphabetically by IP
	sort.SliceStable(ips, func(i, j int) bool {
		ipA := net.ParseIP(ips[i].IP)
		ipB := net.ParseIP(ips[j].IP)
		isA192 := ipA[0] == 192 && ipA[1] == 168
		isB192 := ipB[0] == 192 && ipB[1] == 168

		if isA192 && !isB192 {
			return true // A comes first
		}
		if !isA192 && isB192 {
			return false // B comes first
		}
		// If both are 192.168 or both are not, sort by IP string
		return ips[i].IP < ips[j].IP
	})

	if len(ips) == 0 {
		log.Println("未在接口中找到合适的私有 IP。")
		// Consider adding hostname lookup as a last resort here if needed,
		// but it might not be a private IP suitable for LAN.
	}

	return ips
}

// notify sends a desktop notification using beeep
func notify(title, message, appIcon string) {
	// Use default icon if not provided
	err := beeep.Notify(title, message, appIcon) // beeep handles icon path internally if needed
	if err != nil {
		log.Printf("发送通知失败: %v", err)
		// Don't send another notification about failing notification :)
	}
}

// checkConnectionAndFirewall attempts to connect to the server itself using the currently selected IP
func checkConnectionAndFirewall(port int) {
	ipMutex.RLock()
	host := selectedIP // Use the globally selected IP
	ipMutex.RUnlock()

	if host == "" || host == "0.0.0.0" || host == "127.0.0.1" || strings.HasPrefix(host, "127.") {
		log.Println("\n🔍 [自检] 当前选中IP为空、无效或为本地回环，跳过防火墙代理检测。请通过托盘菜单选择有效IP。")
		return
	}

	url := fmt.Sprintf("http://%s:%d/", host, port)
	log.Printf("\n🔍 [自检] 等待服务器启动 %v, 并尝试连接自身使用选中IP: %s (超时:%v)...", selfCheckWait, url, selfCheckTimeout)
	time.Sleep(selfCheckWait) // Wait for server to likely be up

	client := http.Client{
		Timeout: selfCheckTimeout,
	}

	resp, err := client.Get(url)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			log.Println("\n" + strings.Repeat("!", 60))
			log.Println("!!! ❌ [自检] 连接超时 !!!")
			log.Printf("!!! 服务器无法通过局域网IP %s 访问自己。", host)
			log.Println("!!! 极有可能是【电脑防火墙】阻止了端口 61336 的传入连接。")
			log.Println("!!! ")
			log.Println("!!! 请检查：")
			log.Println("!!!  1. Windows Defender 防火墙 / macOS 防火墙 / 第三方杀毒软件防火墙。")
			log.Println("!!!  2. 在防火墙【入站规则】中【允许】TCP 端口 61336")
			log.Println("!!!  3. 手机和电脑必须在【同一个WiFi/局域网】，关闭手机数据流量/VPN/访客网络。")
			log.Println("!!! 手机访问大概率也会超时 (ERR_CONNECTION_TIMED_OUT)。")
			log.Println(strings.Repeat("!", 60) + "\n")
		} else {
			log.Println("\n" + strings.Repeat("!", 60))
			log.Printf("!!! ⚠️ [自检] 连接错误: %T !!!", err)
			log.Printf("!!! 无法连接到 %s。", url)
			log.Println("!!! 可能原因：")
			log.Println("!!!  1. 服务器启动失败或绑定IP/端口错误 (检查上方日志)。")
			log.Println("!!!  2. 防火墙阻止 (连接被拒绝)。")
			log.Printf("!!!  3. 获取的IP地址 %s 不正确。", host)
			log.Println("!!!  4. 服务器启动太慢，等待时间不足。")
			log.Println(strings.Repeat("!", 60) + "\n")
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Println("✅ [自检] 成功连接到自身。网络和防火墙配置可能正常。请用手机访问。")
	} else {
		log.Printf("⚠️ [自检] 连接自身成功，但返回状态码异常: %d。请检查服务器逻辑。", resp.StatusCode)
	}
}

// onReady runs when the systray is ready.
func onReady() {
	log.Println("Systray onReady 开始执行")
	systray.SetIcon(iconData) // Use embedded icon data
	systray.SetTitle("网页键鼠")  // Optional: Set a title
	systray.SetTooltip("网页键鼠 - 正在启动...")

	// --- Menu items ---
	// Removed: mSelectedIP and mPrimaryIPTitle as per user feedback

	// Sections for IP selection will be populated later
	// Removed separator before IP list as per user feedback
	// We will store references to the IP menu items later
	var primaryIpMenuItems []*systray.MenuItem // Slice for 192.168.x.x items
	var otherIpMenuItems []*systray.MenuItem   // Slice for other IP items (in submenu)
	// Primary IP items will be added dynamically here

	// Placeholder for the submenu for other IPs
	var mOtherIPsSubMenu *systray.MenuItem // Will be created if other IPs exist

	// Removed separator before Startup item

	// --- 开机自启菜单项 (仅 Windows) ---
	var mStartup *systray.MenuItem
	if runtime.GOOS == "windows" {
		mStartup = systray.AddMenuItemCheckbox("开机自启", "启用或禁用开机自启", false) // 初始状态未勾选
		// Removed separator after startup item
	}
	// --- 结束开机自启菜单项 ---

	// mQuit := systray.AddMenuItem("退出", "关闭应用程序") // MOVED TO BOTTOM

	// Goroutine to handle startup menu item clicks (仅 Windows)
	if mStartup != nil { // 确保 mStartup 已初始化 (即在 Windows 上)
		go func() {
			// 先获取一次路径，避免在循环中重复获取
			exePath, err := getExecutablePath()
			if err != nil {
				log.Printf("无法获取可执行文件路径，开机自启功能可能异常: %v", err)
				// 可以在这里禁用菜单项或显示错误状态，但暂时只记录日志
				mStartup.Disable()
				mStartup.SetTooltip(fmt.Sprintf("错误: %v", err))
				return // 如果无法获取路径，则无法进行后续操作
			}

			// 启动时检查并设置初始状态
			enabled, err := isStartupEnabled(startupAppName, exePath)
			if err != nil {
				log.Printf("启动时检查开机自启状态失败: %v", err)
				// 可以在菜单项上提示错误
				mStartup.SetTooltip(fmt.Sprintf("检查状态失败: %v", err))
			} else {
				if enabled {
					mStartup.Check() // 如果已启用，勾选
				} else {
					// 默认启用：如果未启用，尝试启用它
					log.Println("开机自启未启用，尝试默认启用...")
					err := enableStartup(startupAppName, exePath)
					if err != nil {
						log.Printf("默认启用开机自启失败: %v", err)
						notify("开机自启设置失败", fmt.Sprintf("尝试默认启用失败: %v", err), "")
						mStartup.SetTooltip(fmt.Sprintf("默认启用失败: %v", err))
					} else {
						log.Println("默认启用开机自启成功。")
						mStartup.Check() // 启用成功后勾选
					}
				}
			}

			// 循环监听点击事件
			for {
				<-mStartup.ClickedCh
				log.Println("收到开机自启菜单点击事件")
				if mStartup.Checked() { // 如果当前是勾选状态 (表示之前是启用的，用户点击表示要禁用)
					err := disableStartup(startupAppName, exePath)
					if err != nil {
						log.Printf("禁用开机自启失败: %v", err)
						notify("开机自启设置失败", fmt.Sprintf("禁用失败: %v", err), "")
						// 失败时不改变勾选状态
					} else {
						log.Println("用户手动禁用开机自启成功。")
						mStartup.Uncheck() // 禁用成功，取消勾选
					}
				} else { // 如果当前是未勾选状态 (表示之前是禁用的，用户点击表示要启用)
					err := enableStartup(startupAppName, exePath)
					if err != nil {
						log.Printf("启用开机自启失败: %v", err)
						notify("开机自启设置失败", fmt.Sprintf("启用失败: %v", err), "")
						// 失败时不改变勾选状态
					} else {
						log.Println("用户手动启用开机自启成功。")
						mStartup.Check() // 启用成功，勾选
					}
				}
			}
		}()
	}

	// Goroutine to handle quit menu item clicks - MOVED TO BOTTOM
	// go func() {
	// 	<-mQuit.ClickedCh
	// 	log.Println("收到退出菜单点击事件")
	// 	triggerShutdown() // Initiate graceful shutdown
	// }()

	// Start the server in a separate goroutine and update UI once ready
	// Pass references to the menu items that need updating
	// Removed mSelectedIP from parameters
	go startServerAndUpdateUI(&primaryIpMenuItems, &otherIpMenuItems, &mOtherIPsSubMenu) // Pass pointers

	// Quit item logic is now handled within startServerAndUpdateUI to ensure it's added last.

	log.Println("onReady 执行完毕")
}

// startServerAndUpdateUI runs the server setup and updates the systray menu.
// It now takes pointers to slices for primary and other IP menu items, and a pointer to the submenu item pointer.
// Removed mSelectedIP parameter
func startServerAndUpdateUI(primaryIpMenuItems *[]*systray.MenuItem, otherIpMenuItems *[]*systray.MenuItem, mOtherIPsSubMenu **systray.MenuItem) {
	// Get all local IPs
	ipList := getAllLocalIPs()
	port := defaultPort

	// Separate IPs into primary (192.168) and other
	var primaryIPs []IPInfo
	var otherIPs []IPInfo
	for _, ipInfo := range ipList {
		// Force VMware interfaces into 'otherIPs' regardless of IP range
		if strings.Contains(strings.ToLower(ipInfo.InterfaceName), "vmware") {
			otherIPs = append(otherIPs, ipInfo)
			log.Printf("接口 %s (%s) 包含 'vmware'，归类到其他地址。", ipInfo.InterfaceName, ipInfo.IP)
			continue // Skip further checks for this IP
		}

		ip := net.ParseIP(ipInfo.IP)
		// Check if it's a valid IPv4 and starts with 192.168
		if ip != nil && ip.To4() != nil && ip.To4()[0] == 192 && ip.To4()[1] == 168 {
			primaryIPs = append(primaryIPs, ipInfo)
		} else {
			// Check if it's another private range (10.x or 172.16-31.x) or potentially other valid IPs found
			// Note: The original getAllLocalIPs already filters for private ranges,
			// but this check ensures only those intended private ranges (excluding VMware forced above) go here.
			// We might want to be more inclusive here if getAllLocalIPs changes.
			if ip != nil && ip.To4() != nil && (ip.To4()[0] == 10 || (ip.To4()[0] == 172 && ip.To4()[1] >= 16 && ip.To4()[1] <= 31)) {
				otherIPs = append(otherIPs, ipInfo)
			}
			// IPs not matching 192.168, 10.x, 172.16-31.x, or VMware are currently ignored here.
			// If getAllLocalIPs returns other types (e.g., public IPs, though unlikely for local), they won't be listed.
		}
	}

	// Determine default IP (prioritize primary IPs)
	defaultIP := ""
	ipWarning := ""
	if len(primaryIPs) > 0 {
		defaultIP = primaryIPs[0].IP
		log.Printf("默认选择的首选 IP (192.168): %s (来自接口: %s)", defaultIP, primaryIPs[0].InterfaceName)
	} else if len(otherIPs) > 0 {
		defaultIP = otherIPs[0].IP
		log.Printf("无 192.168 IP，默认选择其他私有 IP: %s (来自接口: %s)", defaultIP, otherIPs[0].InterfaceName)
	} else {
		defaultIP = "0.0.0.0" // Fallback if no private IPs found
		ipWarning = "\n      ⚠️  无法自动获取任何局域网IP，请手动查找并检查网络！"
		log.Println("警告: 未找到可用的局域网 IP 地址。")
	}

	// Set the globally selected IP (thread-safe)
	ipMutex.Lock()
	selectedIP = defaultIP
	ipMutex.Unlock()

	// --- Update Console Output ---
	displayIP := defaultIP
	if displayIP == "0.0.0.0" {
		displayIP = "无可用IP"
	}
	accessURL := fmt.Sprintf("http://%s:%d", displayIP, port)

	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("      🖱️  网页键鼠服务器正在启动...  🖱️")
	fmt.Println("      请确保手机和电脑在同一 WiFi/局域网")
	if len(ipList) > 1 {
		fmt.Println("      检测到多个IP地址，请在系统托盘菜单中选择正确的地址:")
		// List primary IPs first
		if len(primaryIPs) > 0 {
			fmt.Println("      主要地址 (192.168.x.x):")
			for _, ipInfo := range primaryIPs {
				fmt.Printf("        - %s (%s)\n", ipInfo.IP, ipInfo.InterfaceName)
			}
		}
		// List other IPs
		if len(otherIPs) > 0 {
			fmt.Println("      其他地址:")
			for _, ipInfo := range otherIPs {
				fmt.Printf("        - %s (%s)\n", ipInfo.IP, ipInfo.InterfaceName)
			}
		}
		fmt.Printf("\n      当前默认选用: %s\n", defaultIP)
	} else if len(ipList) == 1 {
		fmt.Printf("      检测到 IP 地址: %s (%s)\n", ipList[0].IP, ipList[0].InterfaceName)
	}
	fmt.Println("      请在手机浏览器中尝试访问以下地址:")
	fmt.Printf("\n      --->  %s  <---\n", accessURL)
	fmt.Println(ipWarning)
	fmt.Println("\n      日志输出将在下方显示。通过系统托盘图标退出。")
	fmt.Println(strings.Repeat("=", 60))

	// --- Update Systray ---
	tooltip := fmt.Sprintf("网页键鼠 - %s", accessURL)
	systray.SetTooltip(tooltip)

	// Removed updates for mSelectedIP

	// --- Populate Systray IP Menu Items ---
	// Clear existing slices (important if this function were called multiple times, though it's not currently)
	*primaryIpMenuItems = (*primaryIpMenuItems)[:0]
	*otherIpMenuItems = (*otherIpMenuItems)[:0]

	// Helper function for the click handler goroutine
	// This handler now only updates the global IP and the main tooltip
	createClickHandler := func(clickedItem *systray.MenuItem, currentIP string) {
		go func() {
			for {
				<-clickedItem.ClickedCh
				log.Printf("用户选择了 IP: %s", currentIP)

				// Update global selected IP
				ipMutex.Lock()
				selectedIP = currentIP
				ipMutex.Unlock()

				// Update UI elements
				newAccessURL := fmt.Sprintf("http://%s:%d", currentIP, port)
				newTooltip := fmt.Sprintf("网页键鼠 - %s", newAccessURL)
				// Removed updates for mSelectedIP
				systray.SetTooltip(newTooltip)

				// Checkmark logic removed as requested.

				// Optional: Trigger self-check with new IP
				// go checkConnectionAndFirewall(port)
			}
		}()
	}

	// Populate primary IPs (192.168.x.x) directly in the main menu
	for _, ipInfo := range primaryIPs {
		ipStr := ipInfo.IP
		// ifName := ipInfo.InterfaceName // Interface name no longer needed for display text
		menuText := fmt.Sprintf("可尝试IP：%s:%d", ipStr, port)
		// Add normal menu item (no checkbox)
		menuItem := systray.AddMenuItem(menuText, fmt.Sprintf("选择 %s:%d 作为访问地址", ipStr, port))
		*primaryIpMenuItems = append(*primaryIpMenuItems, menuItem) // Store reference
		createClickHandler(menuItem, ipStr)                         // Attach click handler
	}

	// Populate other IPs in a submenu if they exist
	if len(otherIPs) > 0 {
		// Create the submenu if it doesn't exist (it shouldn't on first run, but check defensively)
		if *mOtherIPsSubMenu == nil {
			*mOtherIPsSubMenu = systray.AddMenuItem("其他检测地址", "其他检测到的非 192.168 IP 地址")
		} else {
			// If it somehow exists but was hidden, show it again
			(*mOtherIPsSubMenu).Show()
		}
		// Add items to the submenu
		for _, ipInfo := range otherIPs {
			ipStr := ipInfo.IP
			// ifName := ipInfo.InterfaceName // Interface name no longer needed for display text
			menuText := fmt.Sprintf("可尝试IP：%s:%d", ipStr, port)
			// Add normal submenu item (no checkbox)
			menuItem := (*mOtherIPsSubMenu).AddSubMenuItem(menuText, fmt.Sprintf("选择 %s:%d 作为访问地址", ipStr, port))
			*otherIpMenuItems = append(*otherIpMenuItems, menuItem) // Store reference
			createClickHandler(menuItem, ipStr)                     // Attach click handler
		}
	} else {
		// Optionally hide or disable the submenu placeholder if no other IPs found
		if *mOtherIPsSubMenu != nil {
			(*mOtherIPsSubMenu).Hide() // Or Disable()
		}
	}
	// --- End Systray IP Menu Item Population ---

	// Send startup notification using the default selected IP - Updated text
	notify("网页键鼠已启动", fmt.Sprintf("请访问: %s\n(如无法访问，请右键托盘图标查看其他可尝试IP)", accessURL), "")

	// Start self-check in a goroutine using the initially selected IP
	go checkConnectionAndFirewall(port) // checkConnectionAndFirewall now reads global selectedIP

	// --- Add Quit Item at the very end ---
	systray.AddSeparator() // Separator before Quit
	mQuit := systray.AddMenuItem("退出", "关闭应用程序")
	go func() {
		<-mQuit.ClickedCh
		log.Println("收到退出菜单点击事件")
		triggerShutdown() // Initiate graceful shutdown
	}()
	// --- End Quit Item ---

	// Setup HTTP routes
	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", handleConnections)

	// Create and assign the server instance
	listenAddr := fmt.Sprintf(":%d", port)
	server = &http.Server{Addr: listenAddr} // Assign to global server variable

	// Start the server (blocking call)
	log.Printf("服务器正在监听 %s (绑定到所有接口)", server.Addr)
	err := server.ListenAndServe() // Use the server instance
	if err != nil && err != http.ErrServerClosed {
		// Log non-graceful shutdown errors
		errMsg := fmt.Sprintf("服务器监听错误: %v (端口 %d 可能被占用?)", err, port)
		log.Println(errMsg)
		notify("服务器启动失败", errMsg, "")
		systray.SetTooltip("服务器启动失败!") // Update tooltip on error
		// Optionally trigger shutdown? Or let the app exit?
		// For now, just log and notify. The app might become unresponsive.
		// Consider calling systray.Quit() here if failure is critical.
		// triggerShutdown() // Maybe? To ensure cleanup attempt.
		systray.Quit() // Quit systray loop on critical listen error
	} else if err == http.ErrServerClosed {
		log.Println("服务器已正常关闭。")
	}
}

// onExit runs when the systray is exiting.
func onExit() {
	// Clean up here
	log.Println("Systray onExit 开始执行")
	// Ensure shutdown is triggered if not already done (e.g., via OS signal)
	triggerShutdown()
	log.Println("Systray onExit 执行完毕")
}

// triggerShutdown handles the graceful shutdown of the HTTP server and quits systray.
func triggerShutdown() {
	shutdownOnce.Do(func() {
		log.Println("开始关闭服务器 (triggerShutdown)...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if server != nil {
			if err := server.Shutdown(ctx); err != nil {
				log.Printf("服务器关闭错误: %v", err)
			} else {
				log.Println("服务器已成功关闭。")
			}
		}
		log.Println("请求退出 Systray...")
		systray.Quit() // Signal the main loop to exit
	})
}

// runServer is now deprecated/removed as its logic is moved into startServerAndUpdateUI
/*
func runServer() (string, int) {
	// ... (previous content moved to startServerAndUpdateUI) ...
}
*/

func main() {
	// Setup logging early
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Start the systray loop. onReady will be called when it's ready.
	// onExit will be called when systray.Quit() is called or the process terminates.
	systray.Run(onReady, onExit)

	// Code execution will block here until systray.Quit() is called.
	log.Println("主程序退出。")
}

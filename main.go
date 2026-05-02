package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

const workspaceDir = "workspace"

// serverManager tracks the currently running background server process
var serverManager = struct {
	sync.Mutex
	cmd  *exec.Cmd
	port string
}{}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system environment variables")
	}

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable is required")
	}

	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		log.Fatalf("Failed to create workspace directory: %v", err)
	}
	log.Printf("Workspace directory ready: %s", workspaceDir)

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Panic(err)
	}
	bot.Debug = false
	log.Printf("Authorized on account @%s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil || update.Message.Text == "" {
			continue
		}
		go handleMessage(bot, update.Message)
	}
}

func handleMessage(bot *tgbotapi.BotAPI, message *tgbotapi.Message) {
	text := strings.TrimSpace(message.Text)
	log.Printf("[%s] %s", message.From.UserName, text)

	bot.Send(tgbotapi.NewChatAction(message.Chat.ID, tgbotapi.ChatTyping))

	var replyText string

	switch {
	case isRunCommand(text):
		replyText = handleStartServer()
	case isStopCommand(text):
		replyText = handleStopServer()
	case isStatusCommand(text):
		replyText = handleServerStatus()
	default:
		// Everything else goes to Gemini CLI
		output, err := runGeminiCLI(text)
		if err != nil {
			if output != "" {
				replyText = output
			} else {
				replyText = fmt.Sprintf("⚠️ Error: %v", err)
			}
		} else if output == "" {
			replyText = "✅ Done."
		} else {
			replyText = output
		}
	}

	// Telegram has a 4096 character limit per message
	if len(replyText) > 4096 {
		replyText = replyText[:4090] + "\n[...]"
	}

	msg := tgbotapi.NewMessage(message.Chat.ID, replyText)
	msg.ReplyToMessageID = message.MessageID
	if _, err := bot.Send(msg); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

// --- Server Management ---

func isRunCommand(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{"run the website", "run the server", "start the website", "start the server", "npm start", "npm run", "jalankan website", "jalankan server"}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func isStopCommand(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "stop server") || strings.Contains(lower, "stop the server") ||
		strings.Contains(lower, "kill server") || strings.Contains(lower, "hentikan server")
}

func isStatusCommand(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "server status") || strings.Contains(lower, "is the server running") ||
		strings.Contains(lower, "status server")
}

func handleStartServer() string {
	serverManager.Lock()
	defer serverManager.Unlock()

	// Kill any existing server first
	if serverManager.cmd != nil && serverManager.cmd.Process != nil {
		serverManager.cmd.Process.Kill()
		serverManager.cmd = nil
		log.Println("Killed previous server process")
	}

	// Detect port from package.json (default: 8000)
	port := detectPort()

	// Start npm start as a background process (non-blocking)
	cmd := exec.Command("npm", "start")
	cmd.Dir = workspaceDir

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start server: %v", err)
		return fmt.Sprintf("❌ Failed to start server: %v\n\nMake sure npm is installed and package.json exists in the workspace.", err)
	}

	serverManager.cmd = cmd
	serverManager.port = port

	// Watch for unexpected crash in background
	go func() {
		err := cmd.Wait()
		serverManager.Lock()
		if serverManager.cmd == cmd { // only if it's still our process
			log.Printf("Server process exited: %v", err)
			serverManager.cmd = nil
		}
		serverManager.Unlock()
	}()

	log.Printf("Server started on port %s (PID %d)", port, cmd.Process.Pid)
	return fmt.Sprintf("🚀 Server started!\n\nAccess it at: http://localhost:%s\n\nTo stop the server, send: stop server", port)
}

func handleStopServer() string {
	serverManager.Lock()
	defer serverManager.Unlock()

	if serverManager.cmd == nil || serverManager.cmd.Process == nil {
		return "ℹ️ No server is currently running."
	}

	if err := serverManager.cmd.Process.Kill(); err != nil {
		return fmt.Sprintf("❌ Failed to stop server: %v", err)
	}
	serverManager.cmd = nil
	port := serverManager.port
	serverManager.port = ""
	return fmt.Sprintf("🛑 Server on port %s has been stopped.", port)
}

func handleServerStatus() string {
	serverManager.Lock()
	defer serverManager.Unlock()

	if serverManager.cmd != nil && serverManager.cmd.Process != nil {
		return fmt.Sprintf("✅ Server is running on http://localhost:%s", serverManager.port)
	}
	return "ℹ️ No server is currently running."
}

func detectPort() string {
	data, err := os.ReadFile(workspaceDir + "/package.json")
	if err != nil {
		return "8000"
	}
	content := string(data)
	// Simple scan for port number after -p flag in scripts
	if idx := strings.Index(content, "-p "); idx != -1 {
		rest := content[idx+3:]
		port := ""
		for _, c := range rest {
			if c >= '0' && c <= '9' {
				port += string(c)
			} else if port != "" {
				break
			}
		}
		if port != "" {
			return port
		}
	}
	return "8000"
}

// --- Gemini CLI ---

func runGeminiCLI(input string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// -p = headless/non-interactive mode (required so it doesn't hang)
	// --yolo = auto-approve all tool calls (file write, shell commands, etc.)
	// --resume latest = continue previous session (memory/context)
	cmd := exec.CommandContext(ctx, "gemini", "-p", input, "--yolo", "--resume", "latest")
	cmd.Dir = workspaceDir

	log.Printf("Running gemini: %q", input)

	out, err := cmd.CombinedOutput()
	result := cleanOutput(string(out))

	if ctx.Err() == context.DeadlineExceeded {
		result += "\n\n⏱️ [Timed out after 5 minutes.]"
	}

	return result, err
}

func cleanOutput(raw string) string {
	noisy := []string{
		"Ripgrep is not available. Falling back to GrepTool.",
		"YOLO mode is enabled. All tool calls will be automatically approved.",
		"No previous sessions found for this project.",
		"Warning: 256-color support not detected. Using a terminal with at least 256-color support is recommended for a better visual experience.",
	}

	lines := strings.Split(raw, "\n")
	var cleaned []string
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		skip := false
		for _, n := range noisy {
			if strings.TrimSpace(trimmed) == n {
				skip = true
				break
			}
		}
		if !skip {
			cleaned = append(cleaned, trimmed)
		}
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

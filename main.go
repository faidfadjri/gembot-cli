package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
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
	cmd        *exec.Cmd
	port       string
	projectDir string
}{}

// activeProject tracks the last project gemini was working in
var activeProject struct {
	sync.RWMutex
	dir string // absolute path to the project folder inside workspace
}

// sessionTracker tracks which projects have had at least one message
// during the CURRENT bot run. Resets to empty on every restart.
var sessionTracker = struct {
	sync.Mutex
	projects map[string]bool
}{
	projects: make(map[string]bool),
}

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
	log.Printf("Workspace ready: %s", workspaceDir)

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

	if len(replyText) > 4096 {
		replyText = replyText[:4090] + "\n[...]"
	}

	msg := tgbotapi.NewMessage(message.Chat.ID, replyText)
	msg.ReplyToMessageID = message.MessageID
	if _, err := bot.Send(msg); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

// --- Project Name Extraction ---

var buildPatterns = []*regexp.Regexp{
	// "build/create/make a coffee-website" / "build coffee website project"
	regexp.MustCompile(`(?i)(?:build|create|make|setup|init|start)\s+(?:a\s+|an\s+|the\s+)?([a-zA-Z0-9][\w\s-]{1,40}?)\s+(?:website|web\s*app|app|page|project|site|landing\s*page)`),
	// "coffee website" / "todo app" as standalone phrase
	regexp.MustCompile(`(?i)([a-zA-Z0-9][\w\s-]{1,40}?)\s+(?:website|web\s*app|app|page|project|site)`),
}

// extractProjectSlug tries to derive a filesystem-safe project folder name.
// Returns "" if no project name can be found.
func extractProjectSlug(text string) string {
	for _, re := range buildPatterns {
		m := re.FindStringSubmatch(text)
		if len(m) >= 2 {
			slug := toSlug(m[1])
			if slug != "" && slug != "a" && slug != "the" && slug != "an" {
				return slug
			}
		}
	}
	return ""
}

func toSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// Replace spaces and underscores with hyphens
	s = strings.NewReplacer(" ", "-", "_", "-").Replace(s)
	// Remove non-alphanumeric/hyphen characters
	safe := regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(s, "")
	// Collapse multiple hyphens
	safe = regexp.MustCompile(`-{2,}`).ReplaceAllString(safe, "-")
	return strings.Trim(safe, "-")
}

// resolveProjectDir returns the directory gemini should run in for this prompt.
// If a project name is found → workspace/<slug>/
// Otherwise → last used project, or workspace/ root.
func resolveProjectDir(text string) string {
	slug := extractProjectSlug(text)
	if slug != "" {
		dir := workspaceDir + "/" + slug
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("Warning: could not create project dir %s: %v", dir, err)
			return workspaceDir
		}
		log.Printf("Project dir resolved: %s", dir)
		// Update active project
		activeProject.Lock()
		activeProject.dir = dir
		activeProject.Unlock()
		return dir
	}

	// Fall back to last active project (for follow-up messages like "Yes sure")
	activeProject.RLock()
	last := activeProject.dir
	activeProject.RUnlock()

	if last != "" {
		return last
	}
	return workspaceDir
}

// --- Server Management ---

func isRunCommand(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{
		"run the website", "run the server", "run the app",
		"start the website", "start the server", "start the app",
		"npm start", "npm run",
		"jalankan website", "jalankan server", "jalankan app",
	}
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

	// Determine which project dir to serve
	activeProject.RLock()
	projectDir := activeProject.dir
	activeProject.RUnlock()

	if projectDir == "" {
		projectDir = workspaceDir
	}

	// Kill any existing server
	if serverManager.cmd != nil && serverManager.cmd.Process != nil {
		serverManager.cmd.Process.Kill()
		serverManager.cmd = nil
	}

	port := detectPort(projectDir)

	cmd := exec.Command("npm", "start")
	cmd.Dir = projectDir

	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("❌ Failed to start server: %v\n\nMake sure package.json exists in the project folder.", err)
	}

	serverManager.cmd = cmd
	serverManager.port = port
	serverManager.projectDir = projectDir

	go func() {
		err := cmd.Wait()
		serverManager.Lock()
		if serverManager.cmd == cmd {
			log.Printf("Server exited: %v", err)
			serverManager.cmd = nil
		}
		serverManager.Unlock()
	}()

	log.Printf("Server started at http://localhost:%s (project: %s, PID: %d)", port, projectDir, cmd.Process.Pid)
	return fmt.Sprintf("🚀 Server started!\n\n📁 Project: %s\n🌐 URL: http://localhost:%s\n\nSend \"stop server\" to shut it down.", projectDir, port)
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
	port := serverManager.port
	dir := serverManager.projectDir
	serverManager.cmd = nil
	serverManager.port = ""
	serverManager.projectDir = ""
	return fmt.Sprintf("🛑 Server stopped.\n\n📁 Project: %s\n🌐 Was running on port %s", dir, port)
}

func handleServerStatus() string {
	serverManager.Lock()
	defer serverManager.Unlock()

	if serverManager.cmd != nil && serverManager.cmd.Process != nil {
		return fmt.Sprintf("✅ Server is running\n\n📁 Project: %s\n🌐 http://localhost:%s", serverManager.projectDir, serverManager.port)
	}
	return "ℹ️ No server is currently running."
}

func detectPort(dir string) string {
	data, err := os.ReadFile(dir + "/package.json")
	if err != nil {
		return "8000"
	}
	content := string(data)
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
	projectDir := resolveProjectDir(input)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Determine if this is the first message to this project in the current bot run.
	// First message → fresh session (no --resume).
	// Follow-up messages → resume to maintain context.
	sessionTracker.Lock()
	isFirstMessage := !sessionTracker.projects[projectDir]
	if isFirstMessage {
		sessionTracker.projects[projectDir] = true
	}
	sessionTracker.Unlock()

	args := []string{"-p", input, "--yolo"}
	if !isFirstMessage {
		args = append(args, "--resume", "latest")
	}

	cmd := exec.CommandContext(ctx, "gemini", args...)
	cmd.Dir = projectDir

	if isFirstMessage {
		log.Printf("Running gemini (fresh session) in [%s]: %q", projectDir, input)
	} else {
		log.Printf("Running gemini (resuming session) in [%s]: %q", projectDir, input)
	}

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

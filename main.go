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

	// Keep "typing..." alive every 4 seconds until we have a response.
	// Telegram removes the indicator after ~5s if not refreshed.
	typingDone := make(chan struct{})
	go func() {
		for {
			bot.Send(tgbotapi.NewChatAction(message.Chat.ID, tgbotapi.ChatTyping))
			select {
			case <-typingDone:
				return
			case <-time.After(4 * time.Second):
			}
		}
	}()

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

	close(typingDone) // stop the typing indicator goroutine

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

// buildTriggers requires the message to START with an explicit action verb
// so that regular conversation doesn't get mistaken for project creation.
var buildTriggers = regexp.MustCompile(
	`(?i)^(?:build|create|make|setup|init(?:ialize)?)\s+(?:a\s+|an\s+|me\s+a\s+|me\s+an\s+)?([\w][\w ]{0,24}?)\s+(?:website|web\s*app|webapp|app|page|project|site|landing\s*page)`,
)

// extractProjectSlug tries to derive a filesystem-safe project folder name.
// Only triggers on explicit build/create/make commands at the start of the message.
// Returns "" for regular conversation or ambiguous messages.
func extractProjectSlug(text string) string {
	m := buildTriggers.FindStringSubmatch(strings.TrimSpace(text))
	if len(m) < 2 {
		return ""
	}
	slug := toSlug(m[1])
	// Guard against noise words or single chars
	if len(slug) < 2 || slug == "a" || slug == "an" || slug == "the" || slug == "my" {
		return ""
	}
	// Clamp slug to 3 words max (joined by hyphens)
	parts := strings.SplitN(slug, "-", 4)
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return strings.Join(parts, "-")
}

func toSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer(" ", "-", "_", "-").Replace(s)
	safe := regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(s, "")
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

// noisyExact are lines removed only when they match exactly (trimmed).
var noisyExact = []string{
	"Ripgrep is not available. Falling back to GrepTool.",
	"YOLO mode is enabled. All tool calls will be automatically approved.",
	"No previous sessions found for this project.",
	"Warning: 256-color support not detected. Using a terminal with at least 256-color support is recommended for a better visual experience.",
}

// noisyPrefix are lines removed when they START WITH these strings.
var noisyPrefix = []string{
	"at ",           // JS stack trace frames: "    at Object.<anonymous>"
	"var consoleProcessList", // conpty_console_list_agent.js
	"                 ^",    // JS syntax error caret
	"Node.js v",      // "Node.js v24.14.0"
	"Error: AttachConsole failed", // pty attach error
	"Error executing tool",  // gemini internal tool errors
	"Attempt ",       // "Attempt 1 failed with status 429..."
}

// noisyContains are lines removed when they CONTAIN these substrings.
var noisyContains = []string{
	"conpty_console_list_agent.js",
	"node_modules/@google/gemini-cli",
	"node_modules/@lydell/node-pty",
	"node:internal/",
	"_GaxiosError",
	"retryWithBackoff",
	"streamWithRetries",
	"makeApiCallAndProcessStream",
}

func cleanOutput(raw string) string {
	// Check for rate-limit / capacity errors and replace with friendly message
	if strings.Contains(raw, "No capacity available") || strings.Contains(raw, "RESOURCE_EXHAUSTED") || strings.Contains(raw, "rateLimitExceeded") {
		return "⚠️ Gemini API rate limit reached. Please wait a moment and try again."
	}

	lines := strings.Split(raw, "\n")
	var cleaned []string

	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		trimmedSpace := strings.TrimSpace(trimmed)
		skip := false

		// Exact match
		for _, n := range noisyExact {
			if trimmedSpace == n {
				skip = true
				break
			}
		}

		// Prefix match
		if !skip {
			for _, p := range noisyPrefix {
				if strings.HasPrefix(trimmedSpace, p) {
					skip = true
					break
				}
			}
		}

		// Substring match
		if !skip {
			for _, c := range noisyContains {
				if strings.Contains(trimmed, c) {
					skip = true
					break
				}
			}
		}

		if !skip {
			cleaned = append(cleaned, trimmed)
		}
	}

	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

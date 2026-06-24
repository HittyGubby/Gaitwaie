package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HittyGubby/gaitwaie/internal/config"
	"github.com/HittyGubby/gaitwaie/internal/database"
	"github.com/HittyGubby/gaitwaie/internal/gateway"
	"github.com/HittyGubby/gaitwaie/internal/models"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// TUI states
type tuiState int

const (
	stateNormal tuiState = iota
	stateSingleModelSelect
	statePurgeModelSelect
	stateDeleteConfirm
	stateFetching
)

// Messages for async operations
type testDoneMsg struct {
	keyValue string
	success  bool
	tokens   int
	errMsg   string
}

type modelsFetchedMsg struct {
	alias  string
	models []string
	err    error
}

// displayRow is a single row in the manage TUI table.
type displayRow struct {
	isHeader bool
	alias    string
	baseURL  string
	stats    *models.KeyStats
}

// Styles
var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("57")).
			Padding(0, 1)

	hintStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	providerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12")).
			PaddingLeft(1)

	selectedRowStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("238"))

	modalBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("12")).
				Padding(1, 2)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9"))
)

// manageModel is the Bubble Tea model for the manage TUI.
type manageModel struct {
	cfg        *models.Config
	db         *database.DB
	allStats   []models.KeyStats
	modelCache map[string][]string

	rows           []displayRow
	selectableIdxs []int
	cursorIdx      int

	testResults map[string]string

	state tuiState

	// Model selection modal
	modalAlias  string
	modalModels []string
	modalCursor int
	modalOffset int

	// Delete confirmation
	deleteKey string

	// Purge state
	purgeAliases []string
	purgeIdx     int

	// Fetching state
	fetchForPurge bool

	width    int
	height   int
	quitting bool
	err      error
}

func (m manageModel) Init() tea.Cmd {
	return nil
}

func (m manageModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch m.state {
		case stateNormal:
			return m.updateNormal(msg)
		case stateSingleModelSelect, statePurgeModelSelect:
			return m.updateModelSelect(msg)
		case stateDeleteConfirm:
			return m.updateDeleteConfirm(msg)
		case stateFetching:
			return m, nil
		}

	case testDoneMsg:
		return m.updateTestDone(msg)

	case modelsFetchedMsg:
		return m.updateModelsFetched(msg)
	}

	return m, nil
}

func (m manageModel) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "up", "k":
		if m.cursorIdx > 0 {
			m.cursorIdx--
		}

	case "down", "j":
		if m.cursorIdx < len(m.selectableIdxs)-1 {
			m.cursorIdx++
		}

	case " ":
		if m.cursorIdx >= 0 && m.cursorIdx < len(m.selectableIdxs) {
			row := m.rows[m.selectableIdxs[m.cursorIdx]]
			if row.stats != nil {
				if row.stats.IsActive {
					m.db.DisableKey(row.stats.KeyValue)
				} else {
					m.db.ReenableKey(row.stats.KeyValue)
				}
				m.refreshData()
			}
		}

	case "d":
		if m.cursorIdx >= 0 && m.cursorIdx < len(m.selectableIdxs) {
			row := m.rows[m.selectableIdxs[m.cursorIdx]]
			if row.stats != nil {
				m.deleteKey = row.stats.KeyValue
				m.modalAlias = row.stats.ProviderAlias
				m.state = stateDeleteConfirm
			}
		}

	case "t":
		if m.cursorIdx >= 0 && m.cursorIdx < len(m.selectableIdxs) {
			row := m.rows[m.selectableIdxs[m.cursorIdx]]
			if row.stats != nil {
				m.modalAlias = row.stats.ProviderAlias
				models := m.modelCache[row.stats.ProviderAlias]
				if len(models) == 0 {
					m.fetchForPurge = false
					m.state = stateFetching
					return m, m.fetchModelsCmd(row.stats.ProviderAlias)
				}
				m.modalModels = models
				m.modalCursor = 0
				m.modalOffset = 0
				m.state = stateSingleModelSelect
			}
		}

	case "p":
		m.purgeAliases = m.getProviderAliases()
		m.purgeIdx = 0
		if len(m.purgeAliases) > 0 {
			alias := m.purgeAliases[0]
			m.modalAlias = alias
			models := m.modelCache[alias]
			if len(models) == 0 {
				m.fetchForPurge = true
				m.state = stateFetching
				return m, m.fetchModelsCmd(alias)
			}
			m.modalModels = models
			m.modalCursor = 0
			m.modalOffset = 0
			m.state = statePurgeModelSelect
		}
	}

	return m, nil
}

func (m manageModel) updateModelSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.state = stateNormal
		m.purgeAliases = nil
		m.purgeIdx = 0

	case "up", "k":
		if m.modalCursor > 0 {
			m.modalCursor--
		}
		// scroll offset up
		if m.modalCursor < m.modalOffset {
			m.modalOffset = m.modalCursor
		}

	case "down", "j":
		if m.modalCursor < len(m.modalModels)-1 {
			m.modalCursor++
		}
		// scroll offset down
		maxVisible := m.modalMaxVisible()
		if m.modalCursor >= m.modalOffset+maxVisible {
			m.modalOffset = m.modalCursor - maxVisible + 1
		}

	case "enter":
		if len(m.modalModels) == 0 {
			return m, nil
		}
		selectedModel := m.modalModels[m.modalCursor]

		if m.state == stateSingleModelSelect {
			m.state = stateNormal
			if m.cursorIdx >= 0 && m.cursorIdx < len(m.selectableIdxs) {
				row := m.rows[m.selectableIdxs[m.cursorIdx]]
				if row.stats != nil {
					m.testResults[row.stats.KeyValue] = "testing..."
					return m, m.testKeyCmd(row.stats.KeyValue, m.modalAlias, selectedModel)
				}
			}
		}

		if m.state == statePurgeModelSelect {
			var cmds []tea.Cmd
			for _, idx := range m.selectableIdxs {
				row := m.rows[idx]
				if row.stats != nil && row.stats.ProviderAlias == m.modalAlias {
					m.testResults[row.stats.KeyValue] = "testing..."
					cmds = append(cmds, m.testKeyCmd(row.stats.KeyValue, m.modalAlias, selectedModel))
				}
			}

			m.purgeIdx++
			if m.purgeIdx < len(m.purgeAliases) {
				nextAlias := m.purgeAliases[m.purgeIdx]
				m.modalAlias = nextAlias
				nextModels := m.modelCache[nextAlias]
				if len(nextModels) == 0 {
					m.fetchForPurge = true
					m.state = stateFetching
					return m, tea.Batch(append(cmds, m.fetchModelsCmd(nextAlias))...)
				}
				m.modalModels = nextModels
				m.modalCursor = 0
				m.modalOffset = 0
			} else {
				m.state = stateNormal
				m.purgeAliases = nil
			}

			return m, tea.Batch(cmds...)
		}
	}

	return m, nil
}

func (m manageModel) updateDeleteConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		m.db.SoftDeleteKey(m.deleteKey)
		if err := config.RemoveProviderKey(configPath, m.modalAlias, m.deleteKey); err != nil {
			m.err = fmt.Errorf("update config: %w", err)
		}
		if cfg, err := config.Load(configPath); err == nil {
			m.cfg = cfg
		}
		m.state = stateNormal
		m.refreshData()
		m.deleteKey = ""

	case "n", "esc":
		m.state = stateNormal
		m.deleteKey = ""
	}

	return m, nil
}

func (m manageModel) updateTestDone(msg testDoneMsg) (tea.Model, tea.Cmd) {
	if msg.success {
		m.testResults[msg.keyValue] = fmt.Sprintf("OK (%s tokens)", formatTokens(msg.tokens))
	} else {
		errMsg := msg.errMsg
		if len(errMsg) > 30 {
			errMsg = errMsg[:30] + "..."
		}
		m.testResults[msg.keyValue] = fmt.Sprintf("Error: %s", errMsg)
	}
	return m, nil
}

func (m manageModel) updateModelsFetched(msg modelsFetchedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		m.state = stateNormal
		m.purgeAliases = nil
		return m, nil
	}

	m.modelCache[msg.alias] = msg.models
	m.modalModels = msg.models
	m.modalCursor = 0
	m.modalOffset = 0

	if m.fetchForPurge {
		m.state = statePurgeModelSelect
	} else {
		m.state = stateSingleModelSelect
	}

	return m, nil
}

func (m manageModel) View() string {
	if m.quitting {
		return ""
	}

	if m.width == 0 {
		m.width = 80
	}

	var b strings.Builder

	// Header
	title := headerStyle.Render(" Manage API Keys ")
	hints := hintStyle.Render(" [Space] Toggle  [d] Delete  [t] Test  [p] Purge  [Esc/q] Quit")
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Center, title, hints))
	b.WriteString("\n\n")

	// Column headers
	b.WriteString(m.renderColumnHeaders())
	b.WriteString("\n")

	// Rows
	for i, row := range m.rows {
		selected := !row.isHeader && m.cursorIdx >= 0 && m.cursorIdx < len(m.selectableIdxs) && m.selectableIdxs[m.cursorIdx] == i
		b.WriteString(m.renderRow(row, selected))
		b.WriteString("\n")
	}

	// Bottom area
	b.WriteString("\n")
	switch m.state {
	case stateSingleModelSelect, statePurgeModelSelect:
		b.WriteString(m.renderModelSelectModal())
	case stateDeleteConfirm:
		b.WriteString(m.renderDeleteConfirmModal())
	case stateFetching:
		b.WriteString(hintStyle.Render(" Fetching models..."))
	default:
		if m.err != nil {
			b.WriteString(errorStyle.Render(fmt.Sprintf(" Error: %v", m.err)))
		}
	}

	return b.String()
}

func (m manageModel) computeColWidths() (keyW, statusW, failW, reqW, promptW, complW, totalW, testW int) {
	const maxKeyW = 30
	const minTestW = 12
	spacing := 9 // 7 single-space separators + 1 double-space before TEST

	// Natural widths: start with header label widths
	keyW = 2 + len("KEY") // "  KEY"
	statusW = len("STATUS")
	failW = len("FAILURES")
	reqW = len("REQUESTS")
	promptW = len("PROMPT-TOKEN")
	complW = len("COMPLETION-TOKEN")
	totalW = len("TOTAL-TOKEN")

	// Expand to fit data
	for _, row := range m.rows {
		if row.isHeader || row.stats == nil {
			continue
		}
		s := row.stats
		if w := 2 + len(s.KeyValue); w > keyW {
			keyW = w
		}
		if !s.IsActive && 8 > statusW {
			statusW = 8
		}
		if w := len(strconv.Itoa(s.FailCount)); w > failW {
			failW = w
		}
		if w := len(formatTokens(s.RequestCount)); w > reqW {
			reqW = w
		}
		if w := len(formatTokens(s.PromptTokens)); w > promptW {
			promptW = w
		}
		if w := len(formatTokens(s.CompletionTokens)); w > complW {
			complW = w
		}
		if w := len(formatTokens(s.TotalTokens)); w > totalW {
			totalW = w
		}
	}

	// Cap key column
	if keyW > maxKeyW {
		keyW = maxKeyW
	}

	// TEST column gets all remaining space
	fixedSum := keyW + statusW + failW + reqW + promptW + complW + totalW
	testW = m.width - fixedSum - spacing

	return
}

func (m manageModel) renderColumnHeaders() string {
	keyW, statusW, failW, reqW, promptW, complW, totalW, testW := m.computeColWidths()

	keyCol := lipgloss.NewStyle().Width(keyW).MaxWidth(keyW).Bold(true).Render("  KEY")
	statusCol := lipgloss.NewStyle().Width(statusW).MaxWidth(statusW).Bold(true).Render("STATUS")
	failCol := lipgloss.NewStyle().Width(failW).MaxWidth(failW).Bold(true).Align(lipgloss.Right).Render("FAILURES")
	reqCol := lipgloss.NewStyle().Width(reqW).MaxWidth(reqW).Bold(true).Align(lipgloss.Right).Render("REQUESTS")
	promptCol := lipgloss.NewStyle().Width(promptW).MaxWidth(promptW).Bold(true).Align(lipgloss.Right).Render("PROMPT-TOKEN")
	complCol := lipgloss.NewStyle().Width(complW).MaxWidth(complW).Bold(true).Align(lipgloss.Right).Render("COMPLETION-TOKEN")
	totalCol := lipgloss.NewStyle().Width(totalW).MaxWidth(totalW).Bold(true).Align(lipgloss.Right).Render("TOTAL-TOKEN")
	testCol := lipgloss.NewStyle().Width(testW).MaxWidth(testW).Bold(true).Render("TEST")

	return lipgloss.JoinHorizontal(lipgloss.Top,
		keyCol, " ", statusCol, " ", failCol, " ", reqCol, " ",
		promptCol, " ", complCol, " ", totalCol, "  ", testCol,
	)
}

func (m manageModel) renderRow(row displayRow, selected bool) string {
	if row.isHeader {
		baseURL := row.baseURL
		baseURL = strings.TrimPrefix(baseURL, "https://")
		baseURL = strings.TrimPrefix(baseURL, "http://")
		return providerStyle.Render(fmt.Sprintf("▼ %s (%s)", row.alias, baseURL))
	}

	s := row.stats
	keyW, statusW, failW, reqW, promptW, complW, totalW, testW := m.computeColWidths()

	keyCol := lipgloss.NewStyle().Width(keyW).MaxWidth(keyW).Render("  " + truncateRight(s.KeyValue, keyW-2))

	statusSt := lipgloss.NewStyle().Width(statusW).MaxWidth(statusW)
	var statusCol string
	if s.IsActive {
		statusCol = statusSt.Copy().Foreground(lipgloss.Color("10")).Render("ACTIVE")
	} else {
		statusCol = statusSt.Copy().Foreground(lipgloss.Color("9")).Render("DISABLED")
	}

	failCol := lipgloss.NewStyle().Width(failW).MaxWidth(failW).Align(lipgloss.Right).Render(strconv.Itoa(s.FailCount))
	reqCol := lipgloss.NewStyle().Width(reqW).MaxWidth(reqW).Align(lipgloss.Right).Render(formatTokens(s.RequestCount))
	promptCol := lipgloss.NewStyle().Width(promptW).MaxWidth(promptW).Align(lipgloss.Right).Render(formatTokens(s.PromptTokens))
	complCol := lipgloss.NewStyle().Width(complW).MaxWidth(complW).Align(lipgloss.Right).Render(formatTokens(s.CompletionTokens))
	totalCol := lipgloss.NewStyle().Width(totalW).MaxWidth(totalW).Align(lipgloss.Right).Render(formatTokens(s.TotalTokens))

	testResult := m.testResults[s.KeyValue]
	if testResult == "" {
		testResult = "-"
	}
	testSt := lipgloss.NewStyle().Width(testW).MaxWidth(testW)
	var testCol string
	if strings.HasPrefix(testResult, "testing") {
		testCol = testSt.Copy().Foreground(lipgloss.Color("11")).Render(testResult)
	} else if strings.HasPrefix(testResult, "OK") {
		testCol = testSt.Copy().Foreground(lipgloss.Color("10")).Render(testResult)
	} else if strings.HasPrefix(testResult, "Error") {
		testCol = testSt.Copy().Foreground(lipgloss.Color("9")).Render(testResult)
	} else {
		testCol = testSt.Render(testResult)
	}

	line := lipgloss.JoinHorizontal(lipgloss.Top,
		keyCol, " ", statusCol, " ", failCol, " ", reqCol, " ",
		promptCol, " ", complCol, " ", totalCol, "  ", testCol,
	)

	if selected {
		return selectedRowStyle.Width(m.width).MaxWidth(m.width).Render(line)
	}
	return line
}

func (m manageModel) modalMaxVisible() int {
	overhead := len(m.rows) + 14
	available := m.height - overhead
	if available < 3 {
		available = 3
	}
	if available > len(m.modalModels) {
		available = len(m.modalModels)
	}
	return available
}

func (m manageModel) renderModelSelectModal() string {
	var b strings.Builder

	title := fmt.Sprintf("Select test model for [%s]", m.modalAlias)
	b.WriteString(lipgloss.NewStyle().Bold(true).Render(title))

	maxVisible := m.modalMaxVisible()
	total := len(m.modalModels)
	start := m.modalOffset
	end := start + maxVisible
	if end > total {
		end = total
	}

	if total > maxVisible {
		page := fmt.Sprintf("  (%d-%d of %d)", start+1, end, total)
		b.WriteString(hintStyle.Render(page))
	}
	b.WriteString("\n\n")

	if start > 0 {
		b.WriteString(hintStyle.Render("  ↑ more above\n"))
	}

	for i := start; i < end; i++ {
		model := m.modalModels[i]
		cursor := "  "
		if i == m.modalCursor {
			cursor = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Render("> ")
		}
		line := cursor + model
		if i == m.modalCursor {
			b.WriteString(selectedRowStyle.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	if end < total {
		b.WriteString(hintStyle.Render("  ↓ more below\n"))
	}

	b.WriteString("\n")
	b.WriteString(hintStyle.Render("[Enter] Select  [↑↓] Navigate  [Esc] Cancel"))

	return modalBorderStyle.Render(b.String())
}

func (m manageModel) renderDeleteConfirmModal() string {
	var b strings.Builder

	b.WriteString(lipgloss.NewStyle().Bold(true).Render("Confirm Delete"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("Delete key %s?\n", m.deleteKey))
	b.WriteString("This will soft-delete the key from routing.\n\n")
	b.WriteString(hintStyle.Render("[y] Confirm  [n/Esc] Cancel"))

	return modalBorderStyle.Render(b.String())
}

// Data helpers

func (m *manageModel) refreshData() {
	stats, err := m.db.GetAllKeyStats()
	if err != nil {
		m.err = err
		return
	}
	m.allStats = stats
	m.rows = buildDisplayRows(stats, m.cfg)
	m.selectableIdxs = getSelectableIndices(m.rows)

	validKeys := make(map[string]bool)
	for _, s := range stats {
		validKeys[s.KeyValue] = true
	}
	for k := range m.testResults {
		if !validKeys[k] {
			delete(m.testResults, k)
		}
	}

	if m.cursorIdx >= len(m.selectableIdxs) {
		m.cursorIdx = max(0, len(m.selectableIdxs)-1)
	}
}

func (m manageModel) getProviderAliases() []string {
	seen := make(map[string]bool)
	var aliases []string
	for _, s := range m.allStats {
		if !seen[s.ProviderAlias] {
			seen[s.ProviderAlias] = true
			aliases = append(aliases, s.ProviderAlias)
		}
	}
	return aliases
}

func buildDisplayRows(stats []models.KeyStats, cfg *models.Config) []displayRow {
	var rows []displayRow
	var currentAlias string

	for _, s := range stats {
		if s.ProviderAlias != currentAlias {
			currentAlias = s.ProviderAlias
			baseURL := ""
			if p, ok := cfg.Providers[currentAlias]; ok {
				baseURL = p.BaseURL
			}
			rows = append(rows, displayRow{
				isHeader: true,
				alias:    currentAlias,
				baseURL:  baseURL,
			})
		}
		s := s
		rows = append(rows, displayRow{
			isHeader: false,
			alias:    s.ProviderAlias,
			stats:    &s,
		})
	}

	return rows
}

func getSelectableIndices(rows []displayRow) []int {
	var indices []int
	for i, row := range rows {
		if !row.isHeader {
			indices = append(indices, i)
		}
	}
	return indices
}

// Async commands

func (m manageModel) testKeyCmd(keyValue, alias, model string) tea.Cmd {
	cfg := m.cfg
	db := m.db
	return func() tea.Msg {
		provider := cfg.Providers[alias]
		upstreamURL := strings.TrimRight(provider.BaseURL, "/") + "/chat/completions"

		parts := strings.SplitN(model, "/", 2)
		upstreamModel := model
		if len(parts) > 1 {
			upstreamModel = parts[1]
		}

		body := buildManageTestBody(upstreamModel)
		client := &http.Client{Timeout: 30 * time.Second}

		success, tokens, errMsg := sendManageTestRequest(client, upstreamURL, keyValue, body)

		reqLog := &models.RequestLog{
			Timestamp:        time.Now(),
			StatusCode:       200,
			PromptTokens:     tokens,
			CompletionTokens: tokens,
			TotalTokens:      tokens,
			ProviderAlias:    alias,
			RequestedModel:   model,
			AssignedKey:      keyValue,
			ReceiverName:     "purge",
			IsTestRequest:    true,
		}
		if !success {
			reqLog.StatusCode = 0
		}
		db.InsertRequestLog(reqLog)

		return testDoneMsg{
			keyValue: keyValue,
			success:  success,
			tokens:   tokens,
			errMsg:   errMsg,
		}
	}
}

func (m manageModel) fetchModelsCmd(alias string) tea.Cmd {
	cfg := m.cfg
	db := m.db
	return func() tea.Msg {
		provider := cfg.Providers[alias]
		client := &http.Client{Timeout: 15 * time.Second}

		models, err := gateway.FetchProviderModels(client, provider.BaseURL, provider.Keys, alias)
		if err != nil {
			return modelsFetchedMsg{alias: alias, err: err}
		}

		db.SaveModelCache(alias, models)

		return modelsFetchedMsg{alias: alias, models: models}
	}
}

// Test helpers

func buildManageTestBody(model string) []byte {
	body := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"stream":     true,
		"max_tokens": 5,
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	data, _ := json.Marshal(body)
	return data
}

func sendManageTestRequest(client *http.Client, url, key string, body []byte) (bool, int, string) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return false, 0, fmt.Sprintf("request creation failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := client.Do(req)
	if err != nil {
		return false, 0, fmt.Sprintf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		errMsg := string(bodyBytes)
		if len(errMsg) > 100 {
			errMsg = errMsg[:100]
		}
		return false, 0, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, errMsg)
	}

	scanner := bufio.NewScanner(resp.Body)
	var lastUsageLine string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, `"usage":`) {
			lastUsageLine = line
		}
	}

	promptTokens, _, completionTokens := extractManageUsage(lastUsageLine)
	totalTokens := promptTokens + completionTokens

	if err := scanner.Err(); err != nil && totalTokens == 0 {
		return false, 0, fmt.Sprintf("stream error: %v", err)
	}

	return true, totalTokens, ""
}

func extractManageUsage(line string) (int, int, int) {
	if line == "" {
		return 0, 0, 0
	}

	jsonStr := line
	if strings.HasPrefix(line, "data: ") {
		jsonStr = strings.TrimPrefix(line, "data: ")
	}

	var data struct {
		Usage *struct {
			PromptTokens          int `json:"prompt_tokens"`
			CompletionTokens      int `json:"completion_tokens"`
			PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
			PromptTokensDetails   *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return 0, 0, 0
	}
	if data.Usage == nil {
		return 0, 0, 0
	}

	u := data.Usage
	cached := u.PromptCacheHitTokens
	if cached == 0 && u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}

	billable := u.PromptCacheMissTokens
	if billable == 0 {
		if cached > 0 && u.PromptTokens > cached {
			billable = u.PromptTokens - cached
		} else {
			billable = u.PromptTokens
		}
	}

	return billable, cached, u.CompletionTokens
}

// Utility

func truncateRight(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 4 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Command

func init() {
	rootCmd.AddCommand(manageCmd)
}

var manageCmd = &cobra.Command{
	Use:   "manage",
	Short: "Interactive TUI for managing API keys",
	Long:  `Opens an interactive terminal UI for viewing, enabling/disabling, deleting, and testing API keys.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		db, err := database.Open(cfg.DatabasePath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer db.Close()

		// Sync keys with YAML
		for alias, provider := range cfg.Providers {
			if err := db.SyncKeysExclusive(alias, provider.Keys); err != nil {
				return fmt.Errorf("sync keys for %q: %w", alias, err)
			}
		}

		// Load data
		stats, err := db.GetAllKeyStats()
		if err != nil {
			return fmt.Errorf("get key stats: %w", err)
		}

		modelCache, err := db.GetModelCache()
		if err != nil {
			return fmt.Errorf("get model cache: %w", err)
		}

		m := manageModel{
			cfg:         cfg,
			db:          db,
			allStats:    stats,
			modelCache:  modelCache,
			testResults: make(map[string]string),
			state:       stateNormal,
		}
		m.rows = buildDisplayRows(stats, cfg)
		m.selectableIdxs = getSelectableIndices(m.rows)

		p := tea.NewProgram(m, tea.WithAltScreen())
		_, err = p.Run()
		return err
	},
}

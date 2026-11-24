package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"k8s.io/apimachinery/pkg/types"
)

// The terminal half of pk-metal top: a Bubble Tea model. Update and View are
// plain functions of the model, so tests drive them with fixed messages and
// compare the view at a fixed width; nothing here needs a terminal.

// topSource reads everything the dashboard shows, from caches that watches
// keep current and from the fleet's state endpoint.
type topSource interface {
	Read(ctx context.Context) topInput
}

// topActions are the keys that change something. They call the same code as
// pk-metal approve, hold, unhold and retry.
type topActions interface {
	// Plan returns the plan of an operation as pk-metal approve prints it,
	// who would approve it, and which object it was read from; an error when
	// the caller cannot approve it.
	Plan(ctx context.Context, namespace, name string) (shownPlan, error)
	// Approve approves the operation as the plan's user, if it is still the
	// same object and its plan still reads the same.
	Approve(ctx context.Context, namespace, name string, p shownPlan) (string, error)
	Hold(ctx context.Context, namespace, name string, on bool) (string, error)
	Retry(ctx context.Context, namespace, name string) (string, error)
}

// Messages.
type (
	topDataMsg   topInput
	topTickMsg   struct{}
	topChangeMsg struct{}
	topPlanMsg   struct {
		namespace, name string
		plan            shownPlan
		err             error
	}
	topDoneMsg struct {
		text string
		err  error
	}
)

type topMode int

const (
	modeTiles topMode = iota
	modePlan          // an approval plan is shown and a y/N answer awaited
)

type topModel struct {
	src     topSource
	act     topActions
	changes <-chan struct{} // a watched object changed
	every   time.Duration   // how often the fleet and consoles are read again

	data    topData
	loaded  bool
	reading bool
	width   int
	sel     int
	mode    topMode
	busy    bool // an action runs

	plan             shownPlan
	planNS, planName string
	planAt           time.Time // when the plan was put on the screen
	status           string
	statusBad        bool
	// now is the clock the approval window is measured with; nil is
	// time.Now.
	now func() time.Time
	// actTimeout bounds one key's call to the cluster; zero is
	// topActionTimeout. Tests shorten it.
	actTimeout time.Duration
}

// topActionTimeout is how long a key's call (plan, approve, hold, retry) may
// take. Without a bound a hung API server would leave the screen busy for
// good, and every other key with it.
const topActionTimeout = 30 * time.Second

// bounded runs a key's call with a deadline. A call that runs out says so, and
// that its outcome is not known: an approval may have reached the cluster
// just before the answer was lost.
func (m topModel) bounded(call func(ctx context.Context) tea.Msg) tea.Cmd {
	limit := m.actTimeout
	if limit == 0 {
		limit = topActionTimeout
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		msg := call(ctx)
		if ctx.Err() == nil {
			return msg
		}
		err := fmt.Errorf("the cluster did not answer within %s; the outcome is not known, look again before you act", shortDuration(limit))
		if p, ok := msg.(topPlanMsg); ok {
			p.err = err
			return p
		}
		return topDoneMsg{err: err}
	}
}

// shownPlan is an approval plan as a person sees it: the text, who would
// approve, and the operation object it was read from.
type shownPlan struct {
	Text, User string
	UID        types.UID
}

func (m topModel) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func newTopModel(src topSource, act topActions, changes <-chan struct{}, every time.Duration) topModel {
	return topModel{src: src, act: act, changes: changes, every: every}
}

func (m topModel) Init() tea.Cmd {
	return tea.Batch(m.read(), m.waitChange())
}

// read fetches a new topInput, unless a read is under way.
func (m *topModel) read() tea.Cmd {
	if m.src == nil || m.reading {
		return nil
	}
	m.reading = true
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return topDataMsg(src.Read(ctx))
	}
}

func (m topModel) waitChange() tea.Cmd {
	if m.changes == nil {
		return nil
	}
	ch := m.changes
	return func() tea.Msg {
		<-ch
		return topChangeMsg{}
	}
}

func (m topModel) tick() tea.Cmd {
	if m.every <= 0 {
		return nil
	}
	return tea.Tick(m.every, func(time.Time) tea.Msg { return topTickMsg{} })
}

func (m topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case topDataMsg:
		m.reading, m.loaded = false, true
		m.data = buildTop(topInput(msg))
		if m.sel >= len(m.data.Tiles) {
			m.sel = max(0, len(m.data.Tiles)-1)
		}
		return m, m.tick()
	case topTickMsg:
		return m, m.read()
	case topChangeMsg:
		return m, tea.Batch(m.read(), m.waitChange())
	case topPlanMsg:
		m.busy = false
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
			return m, nil
		}
		m.mode, m.plan, m.planNS, m.planName = modePlan, msg.plan, msg.namespace, msg.name
		m.planAt = m.clock()
		m.status = ""
		return m, nil
	case topDoneMsg:
		m.busy = false
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
		} else {
			m.setStatus(msg.text, false)
		}
		return m, m.read()
	case tea.KeyPressMsg:
		return m.key(msg.String())
	}
	return m, nil
}

func (m *topModel) setStatus(s string, bad bool) {
	m.status, m.statusBad = strings.TrimSpace(s), bad
}

func (m topModel) selected() *tile {
	if m.sel < 0 || m.sel >= len(m.data.Tiles) {
		return nil
	}
	return &m.data.Tiles[m.sel]
}

func (m topModel) key(k string) (tea.Model, tea.Cmd) {
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	if m.mode == modePlan {
		// Only an explicit yes approves; every other key leaves the
		// operation as it was, as pk-metal approve does.
		ns, name, plan := m.planNS, m.planName, m.plan
		m.mode, m.plan = modeTiles, shownPlan{}
		if k != "y" && k != "Y" {
			m.setStatus(errDeclined.Error(), false)
			return m, nil
		}
		// The same window as pk-metal approve's question: a plan left on
		// the screen longer is read again before anything is approved.
		if m.clock().Sub(m.planAt) > approvalWindow {
			m.setStatus(fmt.Sprintf("the plan of %s/%s was shown more than %s ago; nothing changed. Press a to see it as it is now",
				ns, name, shortDuration(approvalWindow)), true)
			return m, nil
		}
		m.busy = true
		m.setStatus("approving "+ns+"/"+name+" …", false)
		act := m.act
		return m, m.bounded(func(ctx context.Context) tea.Msg {
			text, err := act.Approve(ctx, ns, name, plan)
			return topDoneMsg{text, err}
		})
	}
	n := len(m.data.Tiles)
	switch k {
	case "q", "esc":
		return m, tea.Quit
	case "tab", "right", "down", "l", "j":
		if n > 0 {
			m.sel = (m.sel + 1) % n
		}
		return m, nil
	case "shift+tab", "left", "up", "k":
		if n > 0 {
			m.sel = (m.sel + n - 1) % n
		}
		return m, nil
	}
	if m.busy || m.act == nil {
		return m, nil
	}
	t := m.selected()
	if t == nil {
		return m, nil
	}
	act := m.act
	switch k {
	case "a":
		if t.Op == nil || !t.Op.Waits {
			m.setStatus(t.Title+": no operation waits for approval", false)
			return m, nil
		}
		ns, name := t.Op.Namespace, t.Op.Name
		m.busy = true
		m.setStatus("reading the plan of "+ns+"/"+name+" …", false)
		return m, m.bounded(func(ctx context.Context) tea.Msg {
			plan, err := act.Plan(ctx, ns, name)
			return topPlanMsg{namespace: ns, name: name, plan: plan, err: err}
		})
	case "h":
		if t.ClaimName == "" {
			m.setStatus(t.Title+": no claim to hold", false)
			return m, nil
		}
		ns, name, on := t.ClaimNS, t.ClaimName, !t.OnHold
		m.busy = true
		return m, m.bounded(func(ctx context.Context) tea.Msg {
			text, err := act.Hold(ctx, ns, name, on)
			return topDoneMsg{text, err}
		})
	case "r":
		if t.Op == nil || !t.Op.Failed {
			m.setStatus(t.Title+": no failed operation to retry", false)
			return m, nil
		}
		ns, name := t.Op.Namespace, t.Op.Name
		m.busy = true
		return m, m.bounded(func(ctx context.Context) tea.Msg {
			text, err := act.Retry(ctx, ns, name)
			return topDoneMsg{text, err}
		})
	}
	return m, nil
}

// Styles. The marks carry the meaning; colour only repeats it.
var (
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Green)
	styleWait  = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	styleBad   = lipgloss.NewStyle().Foreground(lipgloss.Red).Bold(true)
	styleIdle  = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	styleTitle = lipgloss.NewStyle().Bold(true)
	styleDim   = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	styleTile  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.BrightBlack).Padding(0, 1)
	styleSel   = lipgloss.NewStyle().Border(lipgloss.ThickBorder()).BorderForeground(lipgloss.Cyan).Padding(0, 1)
	styleBox   = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Padding(0, 1)
)

func (s state) style() lipgloss.Style {
	switch s {
	case stOK:
		return styleOK
	case stWait:
		return styleWait
	case stBad:
		return styleBad
	}
	return styleIdle
}

// marked is a fact with its mark, coloured by its state.
func marked(s state, text string) string {
	return s.style().Render(s.mark() + " " + text)
}

// defaultTopWidth is the width the view assumes before the terminal says.
const defaultTopWidth = 100

func (m topModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

// render is the whole screen as text.
func (m topModel) render() string {
	width := m.width
	if width <= 0 {
		width = defaultTopWidth
	}
	var b strings.Builder
	b.WriteString(m.header(width))
	b.WriteString("\n")
	switch {
	case m.mode == modePlan:
		box := styleBox.Width(width)
		body := m.plan.Text + "\n" + styleTitle.Render(fmt.Sprintf("Approve as %s? [y/N]", m.plan.User))
		b.WriteString(box.Render(body))
	case !m.loaded:
		b.WriteString("reading the cluster …")
	case len(m.data.Tiles) == 0:
		b.WriteString("no machines: no Host in the cluster and no fleet machine")
	default:
		b.WriteString(m.grid(width))
	}
	b.WriteString("\n")
	b.WriteString(m.footer(width))
	return b.String()
}

func (m topModel) header(width int) string {
	first := styleTitle.Render("pk-metal top") + "   " + m.data.NodeLine
	if m.data.Attention > 0 {
		first += "   " + styleBad.Render(fmt.Sprintf("needs a person: %d", m.data.Attention))
	} else {
		first += "   " + styleOK.Render("needs a person: 0")
	}
	lines := []string{first}
	if len(m.data.Clusters) == 0 {
		lines = append(lines, styleDim.Render("no TalosCluster"))
	}
	for _, c := range m.data.Clusters {
		avail := marked(c.Available, "Available")
		if c.Why != "" {
			avail += " " + styleWait.Render("("+c.Why+")")
		}
		lines = append(lines, fmt.Sprintf("cluster %s: %s  %s  control planes %d/%d, workers %d/%d",
			c.Name, marked(c.Bootstrapped, "Bootstrapped"), avail,
			c.CPReady, c.ControlPlanes, c.WorkersReady, c.Workers))
	}
	for _, r := range m.data.Rollouts {
		st := stWait
		if r.NeedsAttention {
			st = stBad
		}
		lines = append(lines, fmt.Sprintf("rollout %s (%s, cluster %s): %s  nodes %d/%d",
			r.Name, r.Kind, r.Cluster, marked(st, r.State), r.NodesDone, r.NodesTotal))
	}
	if m.data.FleetLine != "" {
		lines = append(lines, styleDim.Render(m.data.FleetLine))
	}
	if len(m.data.Loading) > 0 {
		lines = append(lines, styleWait.Render("still reading: "+strings.Join(m.data.Loading, ", ")))
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	return strings.Join(lines, "\n")
}

// tileWidth is the outer width of one tile; as many as fit go side by side.
const tileWidth = 50

func (m topModel) grid(width int) string {
	cols := max(1, width/tileWidth)
	w := tileWidth
	if cols == 1 {
		w = width
	}
	var rows []string
	for start := 0; start < len(m.data.Tiles); start += cols {
		var row []string
		for i := start; i < min(start+cols, len(m.data.Tiles)); i++ {
			row = append(row, m.renderTile(m.data.Tiles[i], i == m.sel, w))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, row...))
	}
	return strings.Join(rows, "\n")
}

// consoleLines is how many console lines a tile shows.
const consoleLines = 3

func (m topModel) renderTile(t tile, selected bool, w int) string {
	style := styleTile
	if selected {
		style = styleSel
	}
	inner := w - style.GetHorizontalFrameSize()
	var l []string
	add := func(s string) { l = append(l, ansi.Truncate(s, inner, "…")) }
	label := func(s string) string { return styleDim.Render(fmt.Sprintf("%-6s", s)) }

	title := styleTitle.Render(t.Title)
	if t.Subtitle != "" {
		title += " " + styleDim.Render(t.Subtitle)
	}
	add(title)

	// The flow, BMC → media → VM → node, as marks; the lines below say
	// what each mark stands for. A wide tile has room for the words too.
	var parts []string
	for _, st := range t.Flow {
		part := st.Name + " " + st.State.style().Render(st.State.mark())
		if inner >= 76 {
			part = st.Name + " " + marked(st.State, st.Text)
		}
		parts = append(parts, part)
	}
	add(strings.Join(parts, styleDim.Render(" ─▶ ")))

	bmcLine := orDash(hostPort(t.BMCAddress))
	if t.Sim {
		bmcLine += fmt.Sprintf("  power %s  boot %s", strings.ToLower(t.Power), t.Override)
	} else if t.BMCPhase != "" {
		bmcLine += "  " + t.BMCPhase
	}
	add(label("BMC") + bmcLine)
	if t.Sim {
		if len(t.Media) == 0 {
			add(label("media") + "none")
		}
		for i, md := range t.Media {
			add(label(map[bool]string{true: "media", false: ""}[i == 0]) + md)
		}
		add(label("last") + orDash(t.LastCall))
		vm := t.VM
		if t.HangCycles > 0 {
			vm += " · " + styleBad.Render(fmt.Sprintf("hang-cycled %d×", t.HangCycles))
		}
		add(label("VM") + t.VMState.style().Render(t.VMState.mark()) + " " + vm)
	}

	host := "-"
	if t.Phase != "" {
		host = t.Phase
	}
	if t.BlockedOn != "" {
		host += " · " + t.BlockedOn
	}
	add(label("host") + t.hostState().style().Render(host))
	if t.hostState() == stBad && t.Message != "" {
		add(label("") + styleBad.Render(t.Message))
	}
	claim := "-"
	if t.ClaimName != "" {
		claim = t.ClaimNS + "/" + t.ClaimName
		if t.OnHold {
			claim += "  " + styleWait.Render("ON HOLD")
		}
	}
	add(label("claim") + claim)

	if t.Op == nil {
		add(label("op") + "-")
	} else {
		st := stOK
		switch {
		case t.Op.Failed:
			st = stBad
		case t.Op.Waits:
			st = stWait
		}
		add(label("op") + st.style().Render(t.Op.Step+", "+t.Op.Approval))
		add(label("") + styleDim.Render(t.Op.Namespace+"/"+t.Op.Name))
		if t.Op.Failed && t.Op.Message != "" {
			add(label("") + styleBad.Render(t.Op.Message))
		}
	}

	if t.NodeName == "" {
		add(label("node") + "-")
	} else {
		add(label("node") + strings.TrimSpace(t.NodeName+" "+t.NodeAddress))
		talos := "Talos ready"
		if t.TalosReady != stOK {
			talos = "Talos " + orDash(t.TalosWhy)
		}
		ready := marked(t.TalosReady, talos)
		if t.K8sKnown {
			ready += "  " + marked(t.K8sReady, map[bool]string{true: "kubelet Ready", false: "kubelet not Ready"}[t.K8sReady == stOK])
		}
		add(label("") + ready)
	}

	if t.Sim {
		add(styleDim.Render("console " + orDash(t.ConsoleLog)))
		lines := t.Console
		if len(lines) > consoleLines {
			lines = lines[len(lines)-consoleLines:]
		}
		for i := 0; i < consoleLines; i++ {
			s := ""
			if i < len(lines) {
				s = lines[i]
			}
			add(styleDim.Render("│ ") + s)
		}
	}
	return style.Width(w).Render(strings.Join(l, "\n"))
}

func (m topModel) footer(width int) string {
	keys := "tab select  a approve  h hold/unhold  r retry  q quit"
	if m.mode == modePlan {
		keys = "y approve  any other key: do not approve"
	}
	if m.act == nil {
		keys = "tab select  q quit"
	}
	lines := []string{styleDim.Render(keys)}
	if m.status != "" {
		st := styleOK
		if m.statusBad {
			st = styleBad
		}
		lines = append(lines, st.Render(oneLine(m.status)))
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	return strings.Join(lines, "\n")
}

// oneLine joins the lines of an action's output: the status line has room
// for one.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

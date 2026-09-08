package setup

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/devidevio/gitone/internal/config"
)

// node is one entry of the browsable hierarchy. A node with lexical
// descendants is a real directory the browser can enter and own as
// "path/**"; every other node is an exact-path leaf, an accepted symbolic
// link to a directory included, because the inventory never looks through it.
type node struct {
	name     string
	path     string
	parent   *node
	children []*node
}

func (n *node) directory() bool { return len(n.children) > 0 }

// pattern is the ownership pattern this node stands for.
func (n *node) pattern() string {
	if n.directory() {
		return n.path + "/**"
	}
	return n.path
}

// hierarchy turns the flat relevant inventory into the browsable tree. The
// project files setup assigns itself are left out, exactly as the flat
// suggestions leave them out, and nothing is read from the filesystem again.
// Directories sort before files, both alphabetically.
func hierarchy(relevant []string) *node {
	root := &node{}
	for _, path := range relevant {
		if path == config.PublicFile || path == config.ProjectIgnoreFile {
			continue
		}
		current := root
		for _, name := range strings.Split(path, "/") {
			child := findChild(current, name)
			if child == nil {
				child = &node{name: name, path: strings.TrimPrefix(current.path+"/"+name, "/"), parent: current}
				current.children = append(current.children, child)
			}
			current = child
		}
	}
	sortTree(root)
	return root
}

func findChild(parent *node, name string) *node {
	for _, child := range parent.children {
		if child.name == name {
			return child
		}
	}
	return nil
}

func sortTree(n *node) {
	slices.SortFunc(n.children, func(a, b *node) int {
		if a.directory() != b.directory() {
			if a.directory() {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	for _, child := range n.children {
		sortTree(child)
	}
}

// index records every node by its pattern, which is what tells a
// browser-generated pattern from a custom one.
func index(n *node, known map[string]*node) {
	if n.path != "" {
		known[n.pattern()] = n
	}
	for _, child := range n.children {
		index(child, known)
	}
}

// rowKind is what one line of the current directory view does.
type rowKind int

const (
	// rowEntry is one immediate child of the current directory.
	rowEntry rowKind = iota
	// rowSelf owns the current directory as a whole.
	rowSelf
	// rowCustomHeader labels the custom-pattern section.
	rowCustomHeader
	// rowCustom is one current pattern the hierarchy does not represent.
	rowCustom
	// rowCustomNew opens the custom-pattern input.
	rowCustomNew
)

type row struct {
	kind    rowKind
	node    *node
	pattern string
}

// mark is how one row renders: the checkbox, the trailing explanation and
// whether the row is already owned or locked against changes.
type mark struct {
	box     string
	note    string
	problem bool
	owned   bool
	locked  bool
}

type nodeCounts struct {
	owned       int
	generated   int
	unavailable int
	total       int
}

// browser is the owned-path question of a normal terminal: one Huh field that
// navigates the relevant inventory instead of listing it flat. It keeps the
// answer canonical, so a whole directory never carries generated patterns
// below it, and it never traverses an accepted symbolic link.
type browser struct {
	root     *node
	current  *node
	known    map[string]*node
	owners   map[string]string
	others   []*entry
	relevant []string

	chosen []string
	rows   []row
	cursor int
	saved  map[string]int

	filter    textinput.Model
	filtering bool
	viewport  viewport.Model

	// custom reports that the user asked for the custom-pattern input, which
	// runs outside the form and then reopens this field unchanged.
	custom bool

	err     error
	focused bool
	theme   huh.Theme
	dark    bool
	width   int
	height  int

	keymap huh.MultiSelectKeyMap
	open   key.Binding
	back   key.Binding
}

var _ huh.Field = (*browser)(nil)

func newBrowser(relevant []string, others []*entry, current []string) *browser {
	filter := textinput.New()
	filter.Prompt = "/"

	root := hierarchy(relevant)
	b := &browser{
		root:     root,
		current:  root,
		known:    map[string]*node{},
		owners:   map[string]string{},
		others:   others,
		relevant: relevant,
		chosen:   slices.Clone(current),
		saved:    map[string]int{},
		filter:   filter,
		viewport: viewport.New(),
		keymap:   huh.NewDefaultKeyMap().MultiSelect,
		open:     key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "open")),
		back:     key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "parent")),
	}
	index(root, b.known)
	for _, path := range relevant {
		for _, other := range others {
			if slices.ContainsFunc(other.Paths, func(owned string) bool { return config.Owns(owned, path) }) {
				b.owners[path] = other.name
				break
			}
		}
	}
	b.build()
	return b
}

// set replaces the answer after the custom-pattern input ran, so reopening
// the field keeps the browsing position and the restored cursor.
func (b *browser) set(chosen []string) {
	b.chosen = slices.Clone(chosen)
	b.custom = false
	b.err = nil
	b.build()
}

// answer returns the sorted patterns the user selected.
func (b *browser) answer() []string {
	chosen := slices.Clone(b.chosen)
	slices.Sort(chosen)
	return chosen
}

// covers reports whether the answer already owns one concrete path, through
// an exact pattern, a directory pattern or a custom glob alike.
func (b *browser) covers(path string) bool {
	return slices.ContainsFunc(b.chosen, func(pattern string) bool { return config.Owns(pattern, path) })
}

// covering returns the selected directory pattern of a real ancestor that
// already owns n, or an empty string.
func (b *browser) covering(n *node) string {
	for parent := n.parent; parent != nil && parent.path != ""; parent = parent.parent {
		if pattern := parent.pattern(); slices.Contains(b.chosen, pattern) {
			return pattern
		}
	}
	return ""
}

// counts reports the concrete ownership state at or below n. generated keeps
// browser selections separate from custom-pattern coverage, so only a fully
// generated subtree renders and toggles as fully selected.
func (b *browser) counts(n *node) nodeCounts {
	if !n.directory() {
		counts := nodeCounts{total: 1}
		if b.covers(n.path) {
			counts.owned = 1
		}
		if slices.Contains(b.chosen, n.pattern()) || b.covering(n) != "" {
			counts.generated = 1
		}
		if b.owners[n.path] != "" {
			counts.unavailable = 1
		}
		return counts
	}
	counts := nodeCounts{}
	for _, child := range n.children {
		childCounts := b.counts(child)
		counts.owned += childCounts.owned
		counts.generated += childCounts.generated
		counts.unavailable += childCounts.unavailable
		counts.total += childCounts.total
	}
	return counts
}

// conflicting reports the repository a pattern overlaps, which only an
// ownership correction can still carry into the browser.
func (b *browser) conflicting(pattern string) string {
	if name := used(b.others, pattern); name != "" {
		return name
	}
	return conflict(b.others, b.relevant, pattern)
}

// mark describes how one node renders.
func (b *browser) mark(n *node) mark {
	pattern := n.pattern()
	counts := b.counts(n)
	switch covering := b.covering(n); {
	case slices.Contains(b.chosen, pattern):
		marked := mark{box: "[x]", owned: true}
		if name := b.conflicting(pattern); name != "" {
			marked.note, marked.problem = "conflicts with "+name, true
		}
		return marked
	case !n.directory() && counts.unavailable == 1:
		return mark{box: "[!]", problem: true, locked: true, note: "owned by " + b.owners[n.path]}
	case covering != "":
		return mark{box: "[x]", owned: true, locked: true, note: "covered by " + covering}
	case !n.directory() && counts.owned == 1:
		return mark{box: "[x]", owned: true, locked: true, note: "covered by a custom pattern"}
	case counts.generated == counts.total && counts.total > 0:
		return mark{box: "[x]", owned: true, problem: counts.unavailable > 0, note: describeCounts(counts.owned, counts.unavailable)}
	case counts.owned > 0:
		return mark{box: "[~]", problem: counts.unavailable > 0, note: describeCounts(counts.owned, counts.unavailable)}
	case counts.unavailable > 0:
		return mark{box: "[!]", problem: true, note: describeCounts(counts.owned, counts.unavailable)}
	}
	return mark{box: "[ ]"}
}

func describeCounts(owned, unavailable int) string {
	var parts []string
	if owned > 0 {
		parts = append(parts, fmt.Sprintf("%d selected", owned))
	}
	if unavailable > 0 {
		parts = append(parts, fmt.Sprintf("%d unavailable", unavailable))
	}
	return strings.Join(parts, ", ")
}

// customPatterns are the current patterns the hierarchy does not represent.
func (b *browser) customPatterns() []string {
	var patterns []string
	for _, pattern := range b.chosen {
		if _, generated := b.known[pattern]; !generated {
			patterns = append(patterns, pattern)
		}
	}
	slices.Sort(patterns)
	return patterns
}

func (b *browser) filtered() bool { return b.filter.Value() != "" }

// build renders the current directory into rows. A filter hides the broad
// selection and the custom section, so a filtered select-all can never reach
// a pattern wider than the visible matches.
func (b *browser) build() {
	b.rows = nil
	if !b.filtered() && b.current != b.root {
		b.rows = append(b.rows, row{kind: rowSelf, node: b.current})
	}
	needle := strings.ToLower(b.filter.Value())
	for _, child := range b.current.children {
		if b.filtered() && !strings.Contains(strings.ToLower(child.name), needle) {
			continue
		}
		b.rows = append(b.rows, row{kind: rowEntry, node: child})
	}
	if !b.filtered() {
		if patterns := b.customPatterns(); len(patterns) > 0 {
			b.rows = append(b.rows, row{kind: rowCustomHeader})
			for _, pattern := range patterns {
				b.rows = append(b.rows, row{kind: rowCustom, pattern: pattern})
			}
		}
		b.rows = append(b.rows, row{kind: rowCustomNew})
	}
	b.cursor = min(max(b.cursor, 0), max(len(b.rows)-1, 0))
}

// entry describes one row.
func (b *browser) entry(r row) (mark, string) {
	switch r.kind {
	case rowSelf:
		return b.mark(b.current), "Select this directory: " + b.current.pattern()
	case rowCustomHeader:
		return mark{box: "   ", locked: true}, "Custom patterns"
	case rowCustom:
		marked := mark{box: "[x]", owned: true}
		if name := b.conflicting(r.pattern); name != "" {
			marked.note, marked.problem = "conflicts with "+name, true
		}
		return marked, r.pattern
	case rowCustomNew:
		return mark{box: "   "}, customOption
	default:
		return b.mark(r.node), r.node.pattern()
	}
}

// enter changes the current directory. The filter belongs to one directory,
// so it is cleared, and the cursor of the directory being shown is restored,
// which puts it back on the directory just left.
func (b *browser) enter(target *node) {
	saved := b.cursor
	if target.parent == b.current {
		if position := slices.Index(b.current.children, target); position >= 0 {
			saved = position
			if b.current != b.root {
				saved++
			}
		}
	}
	b.saved[b.current.path] = saved
	b.current = target
	b.clearFilter()
	b.cursor = b.saved[target.path]
	b.build()
}

func (b *browser) clearFilter() {
	b.filter.SetValue("")
	b.setFiltering(false)
}

func (b *browser) setFiltering(filtering bool) {
	b.filtering = filtering
	b.keymap.SetFilter.SetEnabled(filtering)
	b.keymap.Filter.SetEnabled(!filtering)
	b.keymap.Next.SetEnabled(!filtering)
	b.keymap.Submit.SetEnabled(!filtering)
	b.keymap.Prev.SetEnabled(!filtering)
	b.keymap.ClearFilter.SetEnabled(!filtering && b.filtered())
	b.open.SetEnabled(!filtering)
	b.back.SetEnabled(!filtering)
	b.keymap.Toggle.SetEnabled(!filtering)
	b.keymap.SelectAll.SetEnabled(!filtering)
}

// removeBelow drops the pattern of n and every browser-generated pattern
// underneath it. Custom patterns stay, however redundant they become.
func (b *browser) removeBelow(n *node) {
	prefix := n.path + "/"
	b.chosen = slices.DeleteFunc(b.chosen, func(pattern string) bool {
		other, generated := b.known[pattern]
		return generated && (other == n || strings.HasPrefix(other.path, prefix))
	})
}

// selectable reports whether n can be owned without overlapping an earlier
// repository on a path this project actually has.
func (b *browser) selectable(n *node) bool {
	return b.counts(n).unavailable == 0
}

// toggleNode selects or releases one node. A path an ancestor pattern already
// covers cannot be changed here; only the directory the pattern names can
// release it.
func (b *browser) toggleNode(n *node, self bool) {
	if covering := b.covering(n); covering != "" {
		if self {
			b.chosen = slices.DeleteFunc(b.chosen, func(pattern string) bool { return pattern == covering })
		}
		return
	}
	if slices.Contains(b.chosen, n.pattern()) {
		b.removeBelow(n)
		return
	}
	counts := b.counts(n)
	if n.directory() && counts.generated == counts.total {
		b.removeBelow(n)
		return
	}
	if !n.directory() && counts.owned == 1 {
		return
	}
	if !b.selectable(n) {
		if name := b.owners[n.path]; name != "" {
			b.err = fmt.Errorf("%s is owned by repository %q", n.path, name)
			return
		}
		b.err = fmt.Errorf("%s overlaps paths another repository owns", n.pattern())
		return
	}
	b.removeBelow(n)
	b.chosen = append(b.chosen, n.pattern())
}

// selectNode owns a node without releasing anything, which is what select-all
// needs. Actions, covered paths and foreign ownership are skipped silently.
func (b *browser) selectNode(n *node) {
	if b.covering(n) != "" || slices.Contains(b.chosen, n.pattern()) || !b.selectable(n) || (!n.directory() && b.covers(n.path)) {
		return
	}
	b.removeBelow(n)
	b.chosen = append(b.chosen, n.pattern())
}

// row is the row under the cursor, if the filter left any.
func (b *browser) row() (row, bool) {
	if len(b.rows) == 0 {
		return row{}, false
	}
	return b.rows[b.cursor], true
}

func (b *browser) toggle() {
	current, ok := b.row()
	if !ok {
		return
	}
	switch current.kind {
	case rowCustomNew:
		b.custom = true
	case rowCustom:
		b.chosen = slices.DeleteFunc(b.chosen, func(pattern string) bool { return pattern == current.pattern })
	case rowCustomHeader:
	case rowSelf:
		b.toggleNode(b.current, true)
	default:
		b.toggleNode(current.node, false)
	}
}

// selectAll owns the whole current directory when that pattern is available,
// and every selectable immediate entry otherwise. A filter narrows it to the
// visible matches, which it toggles.
func (b *browser) selectAll() {
	if b.filtered() {
		complete := true
		for _, current := range b.rows {
			if current.kind == rowEntry && b.selectable(current.node) && !b.mark(current.node).owned {
				complete = false
				break
			}
		}
		for _, current := range b.rows {
			if current.kind != rowEntry || !b.selectable(current.node) {
				continue
			}
			if complete {
				b.removeBelow(current.node)
				continue
			}
			b.selectNode(current.node)
		}
		return
	}
	if b.current != b.root && b.selectable(b.current) && b.covering(b.current) == "" {
		b.selectNode(b.current)
		return
	}
	for _, child := range b.current.children {
		b.selectNode(child)
	}
}

func (b *browser) validate() error {
	if err := require(b.chosen); err != nil {
		return err
	}
	for _, pattern := range b.answer() {
		if name := b.conflicting(pattern); name != "" {
			return fmt.Errorf("remove %s: it overlaps repository %q", pattern, name)
		}
	}
	return nil
}

// breadcrumb names the directory being shown.
func (b *browser) breadcrumb() string {
	if b.current.path == "" {
		return "/"
	}
	return "/" + b.current.path + "/"
}

func (b *browser) status() string {
	return fmt.Sprintf("%s  %d selected", b.breadcrumb(), len(b.chosen))
}

// Init implements huh.Field.
func (b *browser) Init() tea.Cmd { return nil }

// Update implements huh.Field.
func (b *browser) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	var cmds []tea.Cmd
	if b.filtering {
		var cmd tea.Cmd
		b.filter, cmd = b.filter.Update(msg)
		cmds = append(cmds, cmd)
	}

	switch msg := msg.(type) {
	case tea.BackgroundColorMsg:
		b.dark = msg.IsDark()
	case tea.KeyMsg:
		b.err = nil
		switch {
		case key.Matches(msg, b.keymap.Filter):
			b.setFiltering(true)
			cmds = append(cmds, b.filter.Focus())
		case key.Matches(msg, b.keymap.SetFilter):
			b.setFiltering(false)
		case key.Matches(msg, b.keymap.ClearFilter):
			b.clearFilter()
		case key.Matches(msg, b.keymap.Up):
			if !b.filtering || msg.String() != "k" {
				b.cursor = max(b.cursor-1, 0)
			}
		case key.Matches(msg, b.keymap.Down):
			if !b.filtering || msg.String() != "j" {
				b.cursor = min(b.cursor+1, len(b.rows)-1)
			}
		case key.Matches(msg, b.open):
			if current, ok := b.row(); ok && current.kind == rowEntry && current.node.directory() {
				b.enter(current.node)
			}
		case key.Matches(msg, b.back):
			if b.current.parent != nil {
				b.enter(b.current.parent)
			}
		case key.Matches(msg, b.keymap.Toggle):
			b.toggle()
			if b.custom {
				return b, huh.NextField
			}
		case key.Matches(msg, b.keymap.SelectAll):
			b.selectAll()
		case key.Matches(msg, b.keymap.Prev):
			if b.err = b.validate(); b.err != nil {
				return b, nil
			}
			return b, huh.PrevField
		case key.Matches(msg, b.keymap.Next, b.keymap.Submit):
			if b.err = b.validate(); b.err != nil {
				return b, nil
			}
			return b, huh.NextField
		}
		b.keymap.ClearFilter.SetEnabled(!b.filtering && b.filtered())
		b.build()
	}
	return b, tea.Batch(cmds...)
}

func (b *browser) styles() *huh.FieldStyles {
	theme := b.theme
	if theme == nil {
		theme = huh.ThemeFunc(huh.ThemeCharm)
	}
	if b.focused {
		return &theme.Theme(b.dark).Focused
	}
	return &theme.Theme(b.dark).Blurred
}

func (b *browser) titleView() string {
	styles := b.styles()
	var rendered strings.Builder
	switch {
	case b.filtering:
		rendered.WriteString(b.filter.View())
	case b.filtered():
		rendered.WriteString(styles.Title.Render("Owned paths"))
		rendered.WriteString(styles.Description.Render("/" + b.filter.Value()))
	default:
		rendered.WriteString(styles.Title.Render("Owned paths"))
	}
	if b.err != nil {
		rendered.WriteString(styles.ErrorIndicator.String())
	}
	return rendered.String()
}

func (b *browser) render(position int) string {
	styles := b.styles()
	selector := strings.Repeat(" ", lipgloss.Width(styles.MultiSelectSelector.String()))
	if position == b.cursor && b.focused {
		selector = styles.MultiSelectSelector.String()
	}
	marked, label := b.entry(b.rows[position])
	line := marked.box + " " + label
	switch {
	case marked.locked:
		line = styles.Description.Render(line)
	case marked.owned:
		line = styles.SelectedOption.Render(line)
	default:
		line = styles.UnselectedOption.Render(line)
	}
	if marked.note != "" {
		note := styles.Description.Render("  " + marked.note)
		if marked.problem {
			// The themed error message prefixes its own marker, which would
			// sit in the middle of the line here.
			note = styles.ErrorMessage.UnsetString().Render("  " + marked.note)
		}
		line += note
	}
	return selector + line
}

// View implements huh.Field.
func (b *browser) View() string {
	styles := b.styles()
	header := []string{b.titleView(), styles.Description.Render(b.status())}
	rows := make([]string, len(b.rows))
	cursorOffset := 0
	for position := range b.rows {
		rows[position] = b.render(position)
		if position < b.cursor {
			cursorOffset += lipgloss.Height(rows[position])
		}
	}
	rowView := strings.Join(rows, "\n")
	if b.height > 0 {
		b.viewport.SetWidth(b.width)
		b.viewport.SetHeight(max(b.height-len(header), 0))
		b.viewport.SetContent(rowView)
		cursorHeight := 1
		if len(rows) > 0 {
			cursorHeight = lipgloss.Height(rows[b.cursor])
		}
		switch {
		case cursorOffset < b.viewport.YOffset():
			b.viewport.SetYOffset(cursorOffset)
		case cursorOffset+cursorHeight > b.viewport.YOffset()+b.viewport.Height():
			b.viewport.SetYOffset(cursorOffset + cursorHeight - b.viewport.Height())
		}
		rowView = b.viewport.View()
	}
	content := strings.Join(append(header, rowView), "\n")
	base := styles.Base.Width(b.width)
	if b.height > 0 {
		base = base.Height(b.height)
	}
	return base.Render(content)
}

// flatten lists every node once, directories before files, which is the
// hierarchy without its navigation.
func flatten(n *node, listed []*node) []*node {
	for _, child := range n.children {
		listed = append(listed, child)
		listed = flatten(child, listed)
	}
	return listed
}

// RunAccessible offers the same patterns as one flat numbered list, because a
// screen reader reads that better than a navigable view. A normal terminal
// never reaches it: setup already asks its stable questions whenever live
// rendering is unavailable.
func (b *browser) RunAccessible(w io.Writer, r io.Reader) error {
	reader := bufio.NewReader(r)
	nodes := flatten(b.root, nil)
	for {
		fmt.Fprintf(w, "Owned paths, offered from the files this project has.\n")
		for position, current := range nodes {
			marked := b.mark(current)
			fmt.Fprintf(w, "  %d) %s %s %s\n", position+1, marked.box, current.pattern(), marked.note)
		}
		fmt.Fprintf(w, "  %d) %s\n  0) Confirm selection\n", len(nodes)+1, customOption)

		line, err := reader.ReadString('\n')
		answer := strings.TrimSpace(line)
		if err != nil && answer == "" {
			return err
		}
		choice, convertErr := strconv.Atoi(answer)
		switch {
		case convertErr != nil || choice < 0 || choice > len(nodes)+1:
			fmt.Fprintf(w, "  answer a listed number\n")
		case choice == 0:
			if err := b.validate(); err != nil {
				fmt.Fprintf(w, "  %v\n", err)
				continue
			}
			return nil
		case choice == len(nodes)+1:
			b.custom = true
			return nil
		default:
			b.toggleNode(nodes[choice-1], false)
			if b.err != nil {
				fmt.Fprintf(w, "  %v\n", b.err)
				b.err = nil
			}
		}
	}
}

// Blur implements huh.Field. It never validates, so asking for a custom
// pattern can leave a still empty answer.
func (b *browser) Blur() tea.Cmd { b.focused = false; return nil }

// Focus implements huh.Field.
func (b *browser) Focus() tea.Cmd { b.focused = true; return nil }

// Error implements huh.Field.
func (b *browser) Error() error { return b.err }

// Run implements huh.Field.
func (b *browser) Run() error { return huh.Run(b) }

// Skip implements huh.Field.
func (*browser) Skip() bool { return false }

// Zoom implements huh.Field.
func (*browser) Zoom() bool { return false }

// KeyBinds implements huh.Field.
func (b *browser) KeyBinds() []key.Binding {
	return []key.Binding{
		b.keymap.Up, b.keymap.Down, b.open, b.back, b.keymap.Toggle,
		b.keymap.Filter, b.keymap.SetFilter, b.keymap.ClearFilter,
		b.keymap.SelectAll, b.keymap.Prev, b.keymap.Submit,
	}
}

// WithTheme implements huh.Field.
func (b *browser) WithTheme(theme huh.Theme) huh.Field {
	if b.theme != nil {
		return b
	}
	b.theme = theme
	styles := theme.Theme(b.dark)
	filter := b.filter.Styles()
	filter.Cursor.Color = styles.Focused.TextInput.Cursor.GetForeground()
	filter.Focused.Prompt = styles.Focused.TextInput.Prompt
	filter.Focused.Text = styles.Focused.TextInput.Text
	filter.Focused.Placeholder = styles.Focused.TextInput.Placeholder
	b.filter.SetStyles(filter)
	return b
}

// WithKeyMap implements huh.Field.
func (b *browser) WithKeyMap(keymap *huh.KeyMap) huh.Field {
	b.keymap = keymap.MultiSelect
	b.keymap.Toggle.SetHelp("x", "toggle")
	b.setFiltering(b.filtering)
	return b
}

// WithWidth implements huh.Field.
func (b *browser) WithWidth(width int) huh.Field {
	b.width = width
	b.viewport.SetWidth(width)
	return b
}

// WithHeight implements huh.Field.
func (b *browser) WithHeight(height int) huh.Field {
	b.height = height
	b.viewport.SetHeight(max(height-2, 0))
	return b
}

// WithPosition implements huh.Field.
func (b *browser) WithPosition(position huh.FieldPosition) huh.Field {
	if b.filtering {
		return b
	}
	b.keymap.Prev.SetEnabled(!position.IsFirst())
	b.keymap.Next.SetEnabled(!position.IsLast())
	b.keymap.Submit.SetEnabled(position.IsLast())
	return b
}

// GetKey implements huh.Field.
func (*browser) GetKey() string { return "" }

// GetValue implements huh.Field.
func (b *browser) GetValue() any { return b.answer() }

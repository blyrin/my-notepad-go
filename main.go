package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"github.com/google/uuid"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
	"golang.org/x/text/transform"
)

const appName = "My Notepad Go"

const (
	defaultFontSize   = 14
	defaultFontWeight = 400
)

const (
	endingCRLF = "\r\n"
	endingLF   = "\n"
	endingCR   = "\r"
)

const (
	heavyDocBytes = 50 << 20
	heavyDocLines = 2_000_000
)

var endingLabels = map[string]string{
	endingCRLF: "CRLF",
	endingLF:   "LF",
	endingCR:   "CR",
}

type app struct {
	win    *mygo.Window
	buf    *editor.Buffer
	base   *editor.Buffer
	ed     *editor.State
	path   string
	enc    charset
	ending string
	dark   bool
	theme  bool

	savedStamp fileStamp
	conflict   bool
	asking     bool
	askSpec    askSpec
	pending    notice
	dropped    []string

	mu       sync.Mutex
	filePath string
	fileLn   net.Listener
	fileSock string
	fileFi   os.FileInfo

	ioMu sync.Mutex

	wordWrap     bool
	wrapGuides   bool
	indentGuides bool
	whitespace   bool
	folding      bool
	lineNumbers  bool
	relativeNums bool
	activeLine   bool
	autoClose    bool
	readOnly     bool
	tabSize      int
	wrapMark     int
	fontFamily   string
	fontPick     string
	fontOptions  []string
	fontPicker   bool
	cursorShape  editor.CursorShape

	changeMarks   bool
	scrollPastEnd bool
	multicursor   bool

	fontSize   float32
	fontWeight int

	sizePick      float64
	weightPick    string
	weightOptions []string

	quitting bool
	heavy    bool

	selCache selStats

	encPicker  bool
	encPick    string
	encOptions []string
	encAction  func(charset)

	settingsPath string
}

type settings struct {
	Dark          bool    `json:"dark"`
	FontSize      float32 `json:"fontSize"`
	FontWeight    int     `json:"fontWeight"`
	WordWrap      bool    `json:"wordWrap"`
	WrapGuides    bool    `json:"wrapGuides"`
	IndentGuides  bool    `json:"indentGuides"`
	Whitespace    bool    `json:"whitespace"`
	Folding       bool    `json:"folding"`
	LineNumbers   bool    `json:"lineNumbers"`
	RelativeNums  bool    `json:"relativeNums"`
	ActiveLine    bool    `json:"activeLine"`
	AutoClose     bool    `json:"autoClose"`
	TabSize       int     `json:"tabSize"`
	WrapMark      int     `json:"wrapMark"`
	FontFamily    string  `json:"fontFamily"`
	CursorShape   uint8   `json:"cursorShape"`
	ChangeMarks   bool    `json:"changeMarks"`
	ScrollPastEnd bool    `json:"scrollPastEnd"`
	Multicursor   bool    `json:"multicursor"`
}

type askSpec struct {
	title, detail string
	buttons       []string
	answer        func(int)
}

type notice struct {
	title, text string
}

type selStats struct {
	ver   uint64
	sels  []editor.Selection
	chars int
	lines int
}

type fileStamp struct {
	modTime int64
	size    int64
}

type launch struct {
	files []string
}

type savePlan int

const (
	saveProceed savePlan = iota
	saveAsk
	saveReload
)

func newApp() *app {
	buf := editor.NewBuffer("")
	buf.SetLineEnding("\r\n")
	a := &app{
		buf:           buf,
		base:          editor.NewBuffer(""),
		fontSize:      defaultFontSize,
		fontWeight:    defaultFontWeight,
		fontFamily:    "Consolas",
		wordWrap:      true,
		wrapGuides:    false,
		indentGuides:  true,
		whitespace:    false,
		folding:       true,
		lineNumbers:   true,
		activeLine:    true,
		autoClose:     true,
		changeMarks:   true,
		scrollPastEnd: true,
		multicursor:   true,
		tabSize:       2,
		wrapMark:      120,
		enc:           charsetByID("utf8"),
		ending:        endingCRLF,
	}
	a.loadSettings()
	a.ed = a.newState()
	return a
}

func (a *app) captureSettings() settings {
	return settings{
		Dark: a.dark, FontFamily: a.fontFamily, FontSize: a.fontSize, FontWeight: a.fontWeight,
		WordWrap: a.wordWrap, WrapGuides: a.wrapGuides, IndentGuides: a.indentGuides,
		Whitespace: a.whitespace, Folding: a.folding,
		LineNumbers: a.lineNumbers, RelativeNums: a.relativeNums, ActiveLine: a.activeLine,
		AutoClose: a.autoClose,
		TabSize:   a.tabSize, WrapMark: a.wrapMark, CursorShape: uint8(a.cursorShape),
		ChangeMarks: a.changeMarks, ScrollPastEnd: a.scrollPastEnd, Multicursor: a.multicursor,
	}
}

func (a *app) applySettings(s settings) {
	a.dark = s.Dark
	if s.FontFamily != "" {
		a.fontFamily = s.FontFamily
	}
	if s.FontSize >= 8 && s.FontSize <= 40 {
		a.fontSize = s.FontSize
	}
	if s.FontWeight >= 100 && s.FontWeight <= 900 {
		a.fontWeight = s.FontWeight
	}
	a.wordWrap = s.WordWrap
	a.wrapGuides = s.WrapGuides
	a.indentGuides = s.IndentGuides
	a.whitespace = s.Whitespace
	a.folding = s.Folding
	a.lineNumbers = s.LineNumbers
	a.relativeNums = s.RelativeNums
	a.activeLine = s.ActiveLine
	a.autoClose = s.AutoClose
	switch s.TabSize {
	case 2, 4, 6, 8:
		a.tabSize = s.TabSize
	}
	if s.WrapMark > 0 {
		a.wrapMark = s.WrapMark
	}
	if s.CursorShape <= uint8(editor.CursorUnderline) {
		a.cursorShape = editor.CursorShape(s.CursorShape)
	}
	a.changeMarks = s.ChangeMarks
	a.scrollPastEnd = s.ScrollPastEnd
	a.multicursor = s.Multicursor
}

func (a *app) settingsFile() string {
	if a.settingsPath != "" {
		return a.settingsPath
	}
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return ""
	}
	a.settingsPath = filepath.Join(dir, "settings.json")
	return a.settingsPath
}

func (a *app) loadSettings() {
	path := a.settingsFile()
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var s settings
	if json.Unmarshal(b, &s) != nil {
		return
	}
	a.applySettings(s)
}

func (a *app) saveSettings() {
	path := a.settingsFile()
	if path == "" {
		return
	}
	b, err := json.MarshalIndent(a.captureSettings(), "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}

func (a *app) newState() *editor.State {
	return &editor.State{
		Buffer: a.buf,
		Font:   ui.Font{Family: a.fontFamily, Size: a.fontSize, Weight: a.fontWeight},
	}
}

func (a *app) settings() {
	heavy := a.heavyDoc()
	st := a.ed
	st.SoftWrap = a.wordWrap && !heavy
	st.ReadOnly = a.readOnly
	st.TabSize = a.tabSize
	st.Whitespace = editor.WhitespaceNone
	if a.whitespace {
		st.Whitespace = editor.WhitespaceAll
	}
	st.LineNumbers = editor.LineNumbersOff
	if a.lineNumbers {
		st.LineNumbers = editor.LineNumbersOn
		if a.relativeNums {
			st.LineNumbers = editor.LineNumbersRelative
		}
	}
	st.NoIndentGuides = !a.indentGuides
	st.NoFolding = !a.folding || heavy
	st.NoActiveLine = !a.activeLine
	st.NoAutoClose = !a.autoClose || heavy
	st.NoScrollPastEnd = !a.scrollPastEnd
	st.CursorShape = a.cursorShape
	st.WrapGuides = nil
	if a.wrapGuides {
		st.WrapGuides = []int{a.wrapMark}
	}
	st.DiffBase = nil
	if a.changeMarks && !heavy {
		st.DiffBase = a.base
	}
	st.OnInput = a.onInput
	st.ContextMenu = a.contextMenu
}

type charset struct {
	id, name, key string
	enc           encoding.Encoding
}

var charsetGroups = [][]charset{
	{
		{"utf8", "UTF-8", "U", unicode.UTF8},
		{"utf8bom", "UTF-8 带 BOM", "B", unicode.UTF8BOM},
		{"utf16le", "UTF-16 LE", "L", unicode.UTF16(unicode.LittleEndian, unicode.UseBOM)},
		{"utf16be", "UTF-16 BE", "E", unicode.UTF16(unicode.BigEndian, unicode.UseBOM)},
		{"utf32le", "UTF-32 LE", "3", utf32.UTF32(utf32.LittleEndian, utf32.UseBOM)},
		{"utf32be", "UTF-32 BE", "4", utf32.UTF32(utf32.BigEndian, utf32.UseBOM)},
	},
	{
		{"gb18030", "简体中文 GB18030", "G", simplifiedchinese.GB18030},
		{"gbk", "简体中文 GBK", "K", simplifiedchinese.GBK},
		{"big5", "繁体中文 Big5", "T", traditionalchinese.Big5},
	},
	{
		{"sjis", "日语 Shift_JIS", "S", japanese.ShiftJIS},
		{"eucjp", "日语 EUC-JP", "J", japanese.EUCJP},
		{"iso2022jp", "日语 ISO-2022-JP", "O", japanese.ISO2022JP},
	},
	{
		{"euckr", "韩语 EUC-KR", "R", korean.EUCKR},
	},
	{
		{"cp1252", "西欧 Windows-1252", "W", charmap.Windows1252},
		{"latin1", "西欧 ISO-8859-1", "1", charmap.ISO8859_1},
	},
	{
		{"latin2", "中欧 ISO-8859-2", "2", charmap.ISO8859_2},
		{"cp1250", "中欧 Windows-1250", "P", charmap.Windows1250},
	},
	{
		{"greek", "希腊语 ISO-8859-7", "7", charmap.ISO8859_7},
	},
	{
		{"cyrillic", "西里尔 ISO-8859-5", "C", charmap.ISO8859_5},
		{"cp1251", "西里尔 Windows-1251", "D", charmap.Windows1251},
		{"koi8r", "西里尔 KOI8-R", "Y", charmap.KOI8R},
	},
}

var charsets = func() []charset {
	var all []charset
	for _, group := range charsetGroups {
		all = append(all, group...)
	}
	return all
}()

var (
	charsetUTF8    = charsetByID("utf8")
	charsetGB18030 = charsetByID("gb18030")
	charsetLatin1  = charsetByID("latin1")
)

var charsetMenuIDs = []string{"utf8", "utf8bom", "utf16le", "utf16be", "gb18030"}

var menuCharsets = func() []charset {
	var out []charset
	for _, cs := range charsets {
		if slices.Contains(charsetMenuIDs, cs.id) {
			out = append(out, cs)
		}
	}
	return out
}()

var charsetBOMs = []struct {
	bom  []byte
	name string
}{
	{[]byte{0x00, 0x00, 0xFE, 0xFF}, "UTF-32 BE"},
	{[]byte{0xFF, 0xFE, 0x00, 0x00}, "UTF-32 LE"},
	{[]byte{0xEF, 0xBB, 0xBF}, "UTF-8 带 BOM"},
	{[]byte{0xFE, 0xFF}, "UTF-16 BE"},
	{[]byte{0xFF, 0xFE}, "UTF-16 LE"},
}

func charsetByID(id string) charset {
	for _, cs := range charsets {
		if cs.id == id {
			return cs
		}
	}
	return charset{"utf8", "UTF-8", "U", unicode.UTF8}
}

func charsetByName(name string) charset {
	for _, cs := range charsets {
		if cs.name == name {
			return cs
		}
	}
	return charset{"utf8", "UTF-8", "U", unicode.UTF8}
}

func charsetNames() []string {
	names := make([]string, 0, len(charsets))
	for _, cs := range charsets {
		names = append(names, cs.name)
	}
	return names
}

func detectCharset(data []byte) charset {
	if len(data) == 0 {
		return charsetUTF8
	}
	for _, b := range charsetBOMs {
		if bytes.HasPrefix(data, b.bom) {
			return charsetByName(b.name)
		}
	}
	if utf8.Valid(data) {
		return charsetUTF8
	}
	return detectLegacyCharset(charsetSample(data))
}

func charsetSample(data []byte) []byte {
	const limit = 1 << 16
	if len(data) <= limit {
		return data
	}
	return data[:limit]
}

func detectLegacyCharset(data []byte) charset {
	text, _ := decodeText(charsetGB18030, data)
	if strings.ContainsRune(strings.TrimSuffix(text, string(utf8.RuneError)), utf8.RuneError) {
		return charsetLatin1
	}
	return charsetGB18030
}

func decodeText(cs charset, data []byte) (string, bool) {
	if cs == charsetUTF8 && utf8.Valid(data) {
		return string(data), true
	}
	b, err := cs.enc.NewDecoder().Bytes(data)
	if err != nil {
		return string(data), false
	}
	return string(b), true
}

func encodeText(cs charset, text string) ([]byte, int) {
	var out []byte
	lost := 0
	for text != "" {
		b, n, err := transform.String(cs.enc.NewEncoder(), text)
		n = min(n, len(text))
		out = append(out, b...)
		if err == nil || n >= len(text) {
			return out, lost
		}
		_, size := utf8.DecodeRuneInString(text[n:])
		out = append(out, '?')
		lost++
		text = text[n+max(size, 1):]
	}
	return out, lost
}

func endingName(ending string) string {
	if name, ok := endingLabels[ending]; ok {
		return name
	}
	return "LF"
}

func detectEnding(text string) string {
	crlf := strings.Count(text, "\r\n")
	lf := strings.Count(text, "\n") - crlf
	cr := strings.Count(text, "\r") - crlf
	switch {
	case crlf >= lf && crlf >= cr:
		return endingCRLF
	case cr >= lf:
		return endingCR
	default:
		return endingLF
	}
}

func normalizeEndings(text string) string {
	if !strings.Contains(text, "\r") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	rest := text
	for {
		head, tail, found := strings.Cut(rest, "\r")
		if !found {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(head)
		b.WriteByte('\n')
		rest = strings.TrimPrefix(tail, "\n")
	}
}

func applyEnding(text, ending string) string {
	text = normalizeEndings(text)
	switch ending {
	case endingCRLF:
		return strings.ReplaceAll(text, "\n", "\r\n")
	case endingCR:
		return strings.ReplaceAll(text, "\n", "\r")
	}
	return text
}

func (a *app) setWindow(w *mygo.Window) {
	a.mu.Lock()
	a.win = w
	a.mu.Unlock()
}

func (a *app) window() *mygo.Window {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.win
}

func (a *app) onMain(fn func()) {
	if w := a.window(); w != nil {
		w.Update(fn)
	}
}

func (a *app) setMenu() {
	if w := a.window(); w != nil {
		w.SetMenu(a.buildMenu())
	}
}

func (a *app) windowTitle() string {
	name := "未命名"
	if a.path != "" {
		name = filepath.Base(a.path)
	}
	return name + " - " + appName
}

func (a *app) updateTitle() {
	if w := a.window(); w != nil {
		w.SetTitle(a.windowTitle())
		w.SetDocumentEdited(a.buf.Modified())
	}
}

func (a *app) view(c *ui.Context) {
	a.trackHeavy()
	if a.theme != a.dark {
		a.theme = a.dark
		if a.dark {
			c.SetTheme(ui.DarkTheme())
		} else {
			c.SetTheme(ui.LightTheme())
		}
	}
	a.settings()
	a.shortcuts(c)
	root := ui.Column(c).Fill()
	if files := root.DroppedFiles(); files != nil {
		a.dropped = files
		a.onMain(a.openDropped)
	}
	root.Children(func() {
		editor.View(c, a.ed).Grow(1).FillWidth().AutoFocus()
		a.statusBar(c)
	})
	if a.fontPicker {
		ui.Modal(c, &a.fontPicker, func() { a.fontDialog(c) })
	}
	if a.encPicker {
		ui.Modal(c, &a.encPicker, func() { a.encodingDialog(c) })
	}
	if a.pending.text != "" {
		n := a.pending
		a.pending = notice{}
		c.AddToast(ui.Toast{Title: n.title, Description: n.text})
	}
	if a.asking {
		if i := ui.AlertDialog(c, &a.asking, a.askSpec.title, a.askSpec.detail, a.askSpec.buttons...); i >= 0 {
			answer := a.askSpec.answer
			a.askSpec = askSpec{}
			a.onMain(func() { answer(i) })
		}
	}
}

func (a *app) openDropped() {
	files := a.dropped
	a.dropped = nil
	var openable []string
	for _, f := range files {
		if info, err := os.Stat(f); err != nil || info.IsDir() {
			continue
		}
		openable = append(openable, f)
	}
	if len(openable) == 0 {
		a.notify(notice{appName, "拖入的内容无法作为文件打开。"})
		return
	}
	a.confirmDiscard(func() {
		if len(openable) > 1 {
			spawnInstances(openable[1:])
		}
		a.openPath(openable[0])
	})
}

func (a *app) shortcuts(c *ui.Context) {
	ui.Overlay(c, func() {
		if ui.Box(c.Key("window escape")).OverlayShortcut(0, ui.KeyEscape) {
			if w := a.window(); w != nil {
				w.Close()
			}
		}
	})
	c.OnShortcut(ui.Ctrl, ui.KeyN, a.newFile)
	c.OnShortcut(ui.Ctrl, ui.KeyO, a.openFile)
	c.OnShortcut(ui.Ctrl, ui.KeyS, a.saveFile)
	c.OnShortcut(ui.Ctrl|ui.Shift, ui.KeyS, a.saveFileAs)
	c.OnShortcut(0, ui.KeyF5, a.reload)
	c.OnShortcut(ui.Ctrl, ui.KeyW, func() { a.toggleSetting(&a.wordWrap) })
	c.OnShortcut(ui.Ctrl|ui.Shift|ui.Alt, ui.KeyL, func() { a.toggleSetting(&a.wrapGuides) })
	c.OnShortcut(ui.Ctrl|ui.Shift, ui.KeyG, func() { a.toggleSetting(&a.indentGuides) })
	c.OnShortcut(ui.Ctrl|ui.Shift, ui.Key8, func() { a.toggleSetting(&a.whitespace) })
	c.OnShortcut(ui.Ctrl|ui.Shift, ui.KeyI, func() { a.toggleSetting(&a.activeLine) })
	c.OnShortcut(ui.Ctrl, ui.KeyEqual, func() { a.stepFontSize(1) })
	c.OnShortcut(ui.Ctrl|ui.Shift, ui.KeyEqual, func() { a.stepFontSize(1) })
	c.OnShortcut(ui.Ctrl, ui.KeyMinus, func() { a.stepFontSize(-1) })
	c.OnShortcut(ui.Ctrl, ui.Key0, func() { a.setFontSize(defaultFontSize) })
}

func (a *app) toggleSetting(v *bool) {
	if a.heavyDoc() {
		a.pending = notice{appName, "文档过大，该选项已停用。"}
		return
	}
	*v = !*v
	a.saveSettings()
	a.refreshMenu()
}

func (a *app) onInput(ev ui.InputEvent) bool {
	if ev.Kind == ui.InputScroll && ev.Mods&ui.Ctrl != 0 {
		switch {
		case ev.DY < 0:
			a.stepFontSize(1)
		case ev.DY > 0:
			a.stepFontSize(-1)
		}
		return true
	}
	return !a.multicursor && a.blockMultiCursor(ev)
}

func (a *app) blockMultiCursor(ev ui.InputEvent) bool {
	switch ev.Kind {
	case ui.InputPointerDown:
		return ev.Mods&ui.Alt != 0
	case ui.InputKeyDown:
		switch {
		case ev.Mods&ui.Ctrl != 0 && ev.Mods&ui.Alt != 0 && (ev.Key == ui.KeyUp || ev.Key == ui.KeyDown):
			return true
		case ev.Mods == ui.Ctrl && ev.Key == ui.KeyD:
			return true
		case ev.Mods == ui.Ctrl|ui.Shift && ev.Key == ui.KeyL:
			return true
		}
	default:
		return false
	}
	return false
}

func multiCursorCommand(cmd editor.Command) bool {
	switch cmd {
	case editor.SelectNext, editor.SelectPrevious, editor.SelectAllMatches,
		editor.AddCursorAbove, editor.AddCursorBelow, editor.SplitSelectionIntoLines:
		return true
	default:
		return false
	}
}

func (a *app) contextMenu(m *ui.Menu) {
	if m.Item("复制文件路径").Chosen() {
		a.copyText(a.path)
	}
	if m.Item("插入日期和时间").Chosen() {
		a.insert(time.Now().Format(a.timeFormat()))
	}
}

func (a *app) run(cmd editor.Command) {
	if !a.multicursor && multiCursorCommand(cmd) {
		return
	}
	a.ed.Run(cmd)
	a.redraw()
}

func (a *app) statusBar(c *ui.Context) {
	t := c.Theme()
	pos := a.ed.CursorPos()
	lines := max(a.buf.LineCount(), 1)
	col := pos.Col + 1
	lineLen := utf8.RuneCountInString(a.buf.Line(pos.Line))
	chars, selLines := a.selection()
	lang := "文本文件"
	if l := a.buf.Language(); l != nil {
		lang = l.Name
	}
	volatile := []string{
		fmt.Sprintf("行 %d/%d", pos.Line+1, lines),
		fmt.Sprintf("列 %d/%d", col, lineLen),
		fmt.Sprintf("选定 %d", chars),
		fmt.Sprintf("选行 %d", selLines),
	}
	fixed := []string{
		fmt.Sprintf("%d 字节", a.buf.Len()),
		endingName(a.documentEnding()),
		a.documentCharset().name,
		lang,
	}
	if a.heavyDoc() {
		fixed = append(fixed, "大文件模式")
	}
	if a.conflict {
		volatile = append([]string{"原文件已被其他程序修改"}, volatile...)
	}
	pane := func(items []string, warn bool) {
		for i, s := range items {
			if i > 0 {
				ui.Divider(c).Margin(0, 8)
			}
			text := ui.Text(c, s).FontSize(12)
			if warn && i == 0 {
				text.TextColor(t.Warning)
			} else {
				text.TextColor(t.TextMuted)
			}
		}
	}
	ui.Row(c).Padding(3, 8).AlignItems(ui.Center).Background(t.Surface).
		BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Children(func() {
		pane(volatile, a.conflict)
		ui.Spacer(c)
		pane(fixed, false)
	})
}

func (a *app) selection() (chars, lines int) {
	sels := a.ed.Selections()
	ver := a.buf.Version()
	if a.selCache.ver == ver && slices.Equal(a.selCache.sels, sels) {
		return a.selCache.chars, a.selCache.lines
	}
	snap := a.buf.Snapshot()
	for _, s := range sels {
		lo, hi := s.Anchor, s.Head
		if lo == hi {
			continue
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		snap.Chunks(lo, hi, func(text string) bool {
			chars += utf8.RuneCountInString(text)
			return true
		})
		lines += a.buf.Pos(hi).Line - a.buf.Pos(lo).Line + 1
	}
	a.selCache = selStats{ver: ver, sels: sels, chars: chars, lines: lines}
	return chars, lines
}

func (a *app) redraw() {
	if w := a.window(); w != nil {
		w.Invalidate()
	}
}

func (a *app) refreshMenu() { a.onMain(a.setMenu) }

func (a *app) notify(n notice) { a.onMain(func() { a.pending = n }) }

func (a *app) ask(spec askSpec) {
	a.onMain(func() {
		a.askSpec = spec
		a.asking = true
	})
}

func (a *app) openFontPicker() {
	if len(a.fontOptions) == 0 {
		for _, f := range ui.FontFamilies() {
			a.fontOptions = append(a.fontOptions, f.Name)
		}
	}
	if len(a.weightOptions) == 0 {
		a.weightOptions = fontWeightOptions()
	}
	a.fontPick = a.fontFamily
	a.sizePick = float64(a.fontSize)
	a.weightPick = fontWeightLabel(a.fontWeight)
	a.fontPicker = true
	a.redraw()
}

var fontWeights = []struct {
	value int
	name  string
}{
	{100, "极细"}, {200, "特细"}, {300, "细"}, {400, "常规"}, {500, "中等"},
	{600, "半粗"}, {700, "粗体"}, {800, "特粗"}, {900, "浓黑"},
}

func fontWeightLabel(weight int) string {
	for _, w := range fontWeights {
		if w.value == weight {
			return fmt.Sprintf("%d %s", w.value, w.name)
		}
	}
	return fmt.Sprintf("%d 常规", defaultFontWeight)
}

func fontWeightOptions() []string {
	out := make([]string, 0, len(fontWeights))
	for _, w := range fontWeights {
		out = append(out, fontWeightLabel(w.value))
	}
	return out
}

func parseFontWeight(option string) int {
	label, _, _ := strings.Cut(option, " ")
	value, err := strconv.Atoi(label)
	if err != nil {
		return defaultFontWeight
	}
	for _, w := range fontWeights {
		if w.value == value {
			return value
		}
	}
	return defaultFontWeight
}

func (a *app) fontDialog(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Width(400).Gap(12).Padding(4).Children(func() {
		ui.Text(c, "AaBbCc 0123 { } 预览文本").Font(a.fontPick).FontSize(float32(a.sizePick)).FontWeight(parseFontWeight(a.weightPick)).TextColor(t.TextMuted)
		ui.Combobox(c, &a.fontPick, a.fontOptions).Label("字体")
		ui.NumberInput(c, &a.sizePick, 8, 40, 1).Label("字号")
		ui.Combobox(c, &a.weightPick, a.weightOptions).Label("字重")
		ui.Text(c, "Ctrl+加号 / Ctrl+减号 / Ctrl+0 重置 / Ctrl+滚轮 也可调整字号。").FontSize(12).TextColor(t.TextMuted).SingleLine()
		ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
			if ui.Button(c, "取消").Clicked() {
				a.fontPicker = false
			}
			if ui.PrimaryButton(c, "确定").Clicked() {
				if slices.Contains(a.fontOptions, a.fontPick) {
					a.setFontFamily(a.fontPick)
				}
				a.setFontSize(float32(a.sizePick))
				a.setFontWeight(parseFontWeight(a.weightPick))
				a.fontPicker = false
			}
		})
	})
}

func (a *app) pickCharset(fn func(charset)) {
	if len(a.encOptions) == 0 {
		a.encOptions = charsetNames()
	}
	a.encPick = a.documentCharset().name
	a.encAction = fn
	a.encPicker = true
	a.redraw()
}

func (a *app) encodingDialog(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Width(360).Gap(12).Padding(4).Children(func() {
		ui.Text(c, "从完整列表中选择用于读取或写入文件的字符编码。").FontSize(12).TextColor(t.TextMuted).SingleLine()
		ui.Combobox(c, &a.encPick, a.encOptions).Label("编码")
		ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
			if ui.Button(c, "取消").Clicked() {
				a.encPicker = false
			}
			if ui.PrimaryButton(c, "确定").Clicked() {
				cs := charsetByName(a.encPick)
				fn := a.encAction
				a.encPicker, a.encAction = false, nil
				if fn != nil {
					fn(cs)
				}
			}
		})
	})
}

func (a *app) stepFontSize(delta float32) {
	a.setFontSize(a.fontSize + delta)
}

func (a *app) setFontSize(size float32) {
	size = max(8, min(40, float32(int(size))))
	if size == a.fontSize {
		return
	}
	a.fontSize = size
	a.saveSettings()
	a.applyFont()
}

func (a *app) setFontWeight(weight int) {
	if weight == a.fontWeight {
		return
	}
	a.fontWeight = weight
	a.saveSettings()
	a.applyFont()
}

func (a *app) setFontFamily(family string) {
	if family == "" || family == a.fontFamily {
		return
	}
	a.fontFamily = family
	a.saveSettings()
	a.applyFont()
}

func (a *app) applyFont() {
	a.ed.Font = ui.Font{Family: a.fontFamily, Size: a.fontSize, Weight: a.fontWeight}
	a.redraw()
}

func (a *app) item(label, accel string, fn func()) *mygo.MenuItem {
	return &mygo.MenuItem{Label: label, Accelerator: accel, Click: func(*mygo.MenuItem, *mygo.Window) { fn() }}
}

func (a *app) check(label, accel string, on bool, fn func(bool)) *mygo.MenuItem {
	return &mygo.MenuItem{Label: label, Accelerator: accel, Type: mygo.MenuItemCheckbox, Checked: on,
		Click: func(it *mygo.MenuItem, _ *mygo.Window) {
			fn(it.IsChecked())
			a.saveSettings()
			a.refreshMenu()
		}}
}

func (a *app) heavyCheck(label, accel string, on bool, fn func(bool)) *mygo.MenuItem {
	it := a.check(label, accel, on, fn)
	it.Disabled = a.heavyDoc()
	return it
}

func disableIf(heavy bool, items ...*mygo.MenuItem) []*mygo.MenuItem {
	if heavy {
		for _, it := range items {
			it.Disabled = true
		}
	}
	return items
}

func (a *app) command(label, accel string, cmd editor.Command) *mygo.MenuItem {
	return a.item(label, accel, func() { a.run(cmd) })
}

func sub(label string, items ...*mygo.MenuItem) *mygo.MenuItem {
	return &mygo.MenuItem{Label: label, Submenu: items}
}

func (a *app) buildMenu() *mygo.Menu {
	heavy := a.heavyDoc()
	return mygo.NewMenu([]*mygo.MenuItem{
		sub("文件(&F)",
			a.item("新建(&N)", "Ctrl+N", a.newFile),
			a.item("打开(&O)...", "Ctrl+O", a.openFile),
			mygo.Separator(),
			a.item("保存(&S)", "Ctrl+S", a.saveFile),
			a.item("另存为(&A)...", "Ctrl+Shift+S", a.saveFileAs),
			a.item("从文件还原(&V)", "F5", a.reload),
			mygo.Separator(),
			a.check("只读(&R)", "", a.readOnly, func(on bool) { a.readOnly = on }),
			mygo.Separator(),
			sub("复制路径(&D)",
				a.item("完整路径(&F)", "", func() { a.copyText(a.clipPath("full")) }),
				a.item("文件名(&N)", "", func() { a.copyText(a.clipPath("name")) }),
				a.item("所属目录(&D)", "", func() { a.copyText(a.clipPath("dir")) }),
			),
			mygo.Separator(),
			sub("编码(&B)", a.encodingItems()...),
			sub("换行符(&E)", a.endingItems()...),
			mygo.Separator(),
			a.item("退出(&X)", "Alt+F4", a.quit),
		),
		sub("编辑(&E)",
			&mygo.MenuItem{Label: "撤销(&U)", Role: mygo.RoleUndo, Accelerator: "Ctrl+Z"},
			&mygo.MenuItem{Label: "重做(&R)", Role: mygo.RoleRedo, Accelerator: "Ctrl+Y"},
			mygo.Separator(),
			&mygo.MenuItem{Label: "剪切(&T)", Role: mygo.RoleCut, Accelerator: "Ctrl+X"},
			&mygo.MenuItem{Label: "复制(&C)", Role: mygo.RoleCopy, Accelerator: "Ctrl+C"},
			&mygo.MenuItem{Label: "粘贴(&P)", Role: mygo.RolePaste, Accelerator: "Ctrl+V"},
			&mygo.MenuItem{Label: "删除(&D)", Role: mygo.RoleDelete, Accelerator: "Delete"},
			mygo.Separator(),
			&mygo.MenuItem{Label: "全选(&A)", Role: mygo.RoleSelectAll, Accelerator: "Ctrl+A"},
			mygo.Separator(),
			sub("行(&L)",
				a.command("缩进(&I)", "Ctrl+]", editor.Indent),
				a.command("减少缩进(&U)", "Ctrl+[", editor.Outdent),
				mygo.Separator(),
				a.command("上移(&O)", "Alt+Up", editor.MoveLinesUp),
				a.command("下移(&D)", "Alt+Down", editor.MoveLinesDown),
				a.command("复制行(&C)", "Alt+Shift+Up", editor.DuplicateLinesUp),
				a.command("删除行(&R)", "Ctrl+Shift+K", editor.DeleteLine),
				a.command("合并行(&J)", "Ctrl+J", editor.JoinLines),
				mygo.Separator(),
				a.command("排序(&S)", "", editor.SortLines),
				a.command("忽略大小写排序(&G)", "", editor.SortLinesCaseInsensitive),
				a.command("去重(&Q)", "", editor.UniqueLines),
				a.command("删除行尾空白(&W)", "", editor.TrimTrailingWhitespace),
			),
			sub("选区(&S)",
				a.command("上方增加光标(&U)", "Ctrl+Alt+Up", editor.AddCursorAbove),
				a.command("下方增加光标(&D)", "Ctrl+Alt+Down", editor.AddCursorBelow),
				mygo.Separator(),
				a.command("选择下一个匹配(&N)", "Ctrl+D", editor.SelectNext),
				a.command("选中全部匹配(&A)", "Ctrl+Shift+L", editor.SelectAllMatches),
				mygo.Separator(),
				a.command("扩大选区(&G)", "Alt+Shift+Right", editor.SelectLarger),
				a.command("缩小选区(&L)", "Alt+Shift+Left", editor.SelectSmaller),
				mygo.Separator(),
				a.command("删除左侧单词", "Ctrl+Backspace", editor.DeleteWordLeft),
				a.command("删除右侧单词", "Ctrl+Delete", editor.DeleteWordRight),
				a.command("选择左侧单词", "Ctrl+Shift+Left", editor.SelectWordLeft),
				a.command("选择右侧单词", "Ctrl+Shift+Right", editor.SelectWordRight),
			),
			sub("注释(&N)",
				a.command("切换行注释(&T)", "Ctrl+/", editor.ToggleComment),
				a.command("切换块注释(&K)", "Ctrl+Shift+A", editor.ToggleBlockComment),
			),
			sub("转换(&V)",
				a.command("转换为大写(&U)", "", editor.Uppercase),
				a.command("转换为小写(&L)", "", editor.Lowercase),
				a.command("转换为标题大小写(&T)", "", editor.TitleCase),
				mygo.Separator(),
				a.item("Base64 编码(&E)", "", func() {
					a.transform(func(s string) string {
						return base64.StdEncoding.EncodeToString([]byte(s))
					})
				}),
				a.item("Base64 解码(&D)", "", func() {
					a.transform(func(s string) string {
						b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
						if err != nil {
							return s
						}
						return string(b)
					})
				}),
				a.item("URL 编码(&N)", "", func() { a.transform(url.QueryEscape) }),
				a.item("URL 解码(&R)", "", func() {
					a.transform(func(s string) string {
						out, err := url.QueryUnescape(s)
						if err != nil {
							return s
						}
						return out
					})
				}),
			),
			sub("插入(&I)",
				a.item("日期和时间(&D)", "", func() { a.insert(time.Now().Format(a.timeFormat())) }),
				mygo.Separator(),
				a.item("文件名(&F)", "", func() { a.insert(a.baseName()) }),
				a.item("文件路径(&P)", "", func() { a.insert(a.path) }),
			),
		),
		sub("查找(&R)",
			a.command("查找(&F)...", "Ctrl+F", editor.Find),
			a.command("查找下一个(&N)", "F3", editor.FindNext),
			a.command("查找上一个(&P)", "Shift+F3", editor.FindPrevious),
			mygo.Separator(),
			a.command("使用所选内容查找(&E)", "Ctrl+E", editor.UseSelectionForFind),
			a.command("在选区内查找(&X)", "Ctrl+Alt+L", editor.FindInSelection),
			a.command("替换(&R)...", "Ctrl+H", editor.FindReplace),
			mygo.Separator(),
			a.command("跳转到(&G)...", "Ctrl+G", editor.GoToLine),
			a.command("转到文件开头(&H)", "Ctrl+Home", editor.MoveDocStart),
			a.command("转到文件结尾(&D)", "Ctrl+End", editor.MoveDocEnd),
		),
		sub("查看(&V)",
			a.heavyCheck("自动换行(&R)", "Ctrl+W", a.wordWrap, func(on bool) { a.wordWrap = on }),
			mygo.Separator(),
			sub("行号(&N)", a.lineNumItems()...),
			a.check("显示空白字符(&W)", "Ctrl+Shift+8", a.whitespace, func(on bool) { a.whitespace = on }),
			a.check("显示缩进提示(&A)", "Ctrl+Shift+G", a.indentGuides, func(on bool) { a.indentGuides = on }),
			a.check("显示行长度标记(&L)", "Ctrl+Shift+Alt+L", a.wrapGuides, func(on bool) { a.wrapGuides = on }),
			a.check("高亮当前行(&G)", "Ctrl+Shift+I", a.activeLine, func(on bool) { a.activeLine = on }),
			a.check("允许滚动超过末尾(&P)", "", a.scrollPastEnd, func(on bool) { a.scrollPastEnd = on }),
			mygo.Separator(),
			a.heavyCheck("代码折叠(&F)", "", a.folding, func(on bool) { a.folding = on }),
			sub("折叠(&O)", disableIf(heavy,
				a.command("折叠(&F)", "Ctrl+Shift+[", editor.Fold),
				a.command("展开(&U)", "Ctrl+Shift+]", editor.Unfold),
				a.command("切换折叠(&T)", "", editor.ToggleFold),
				mygo.Separator(),
				a.command("递归折叠(&R)", "", editor.FoldRecursive),
				a.command("递归展开(&E)", "", editor.UnfoldRecursive),
				mygo.Separator(),
				a.command("折叠全部(&A)", "", editor.FoldAll),
				a.command("展开全部(&L)", "", editor.UnfoldAll),
			)...),
			mygo.Separator(),
			sub("变更记录标记(&H)", disableIf(heavy,
				a.heavyCheck("显示变更记录标记(&S)", "", a.changeMarks, func(on bool) { a.changeMarks = on }),
				mygo.Separator(),
				a.command("上一处更改(&V)", "Alt+Shift+F5", editor.PreviousHunk),
				a.command("下一处更改(&X)", "Alt+F5", editor.NextHunk),
				a.command("还原此更改(&Z)", "Ctrl+Alt+Z", editor.RevertHunk),
			)...),
			a.item("清除修改/撤销历史(&C)", "", a.clearHistory),
		),
		sub("外观(&P)",
			a.item("字体(&F)...", "", a.openFontPicker),
			sub("语法高亮方案集(&S)", disableIf(heavy, a.languageItems()...)...),
			sub("光标形状(&C)", a.cursorItems()...),
			mygo.Separator(),
			a.check("Windows 深色模式(&D)", "", a.dark, func(on bool) { a.dark = on }),
		),
		sub("设置(&S)",
			sub("制表符设置(&T)", a.tabItems()...),
			a.item("将制表符替换为空格(&R)", "", a.tabsToSpaces),
			mygo.Separator(),
			a.heavyCheck("自动关闭括号与引号(&Q)", "", a.autoClose, func(on bool) { a.autoClose = on }),
			a.check("启用多重光标选定(&M)", "", a.multicursor, func(on bool) { a.multicursor = on }),
			mygo.Separator(),
			a.check("劫持系统记事本(&S)", "", notepadHooked(), func(on bool) {
				if err := setNotepadHook(on); err != nil {
					a.notify(notice{"劫持系统记事本失败", err.Error()})
				}
			}),
			a.check("右键菜单“My Notepad Go 打开”(&C)", "", shellMenuHooked(), func(on bool) {
				if err := setShellMenu(on); err != nil {
					a.notify(notice{"设置右键菜单失败", err.Error()})
				}
			}),
		),
	})
}

func (a *app) endingItems() []*mygo.MenuItem {
	ending := a.documentEnding()
	item := func(label, value string) *mygo.MenuItem {
		return &mygo.MenuItem{Label: label, Type: mygo.MenuItemRadio, Checked: ending == value,
			Click: func(*mygo.MenuItem, *mygo.Window) { a.setEnding(value) }}
	}
	return []*mygo.MenuItem{
		item("CR+LF(&C)", endingCRLF), item("LF(&L)", endingLF), item("CR(&R)", endingCR),
		mygo.Separator(),
		a.item("统一换行符(&U)", "", a.unifyLineEnding),
	}
}

func (a *app) encodingItems() []*mygo.MenuItem {
	current := a.documentCharset()
	var out []*mygo.MenuItem
	for _, cs := range menuCharsets {
		out = append(out, &mygo.MenuItem{
			Label: fmt.Sprintf("%s(&%s)", cs.name, cs.key), Type: mygo.MenuItemRadio, Checked: current.id == cs.id,
			Click: func(*mygo.MenuItem, *mygo.Window) { a.setCharset(cs) },
		})
	}
	out = append(out, mygo.Separator())
	out = append(out,
		a.item("更多编码(&M)...", "", func() { a.pickCharset(a.setCharset) }),
		mygo.Separator(),
		a.item("重新载入为(&R)...", "", func() { a.pickCharset(a.reloadCharsetAs) }),
		a.item("保存为(&S)...", "", func() { a.pickCharset(a.saveCharsetAs) }),
	)
	return out
}

func (a *app) tabItems() []*mygo.MenuItem {
	var out []*mygo.MenuItem
	for _, size := range []int{2, 4, 6, 8} {
		out = append(out, &mygo.MenuItem{
			Label: fmt.Sprintf("%d(&%d)", size, size), Type: mygo.MenuItemRadio, Checked: a.tabSize == size,
			Click: func(*mygo.MenuItem, *mygo.Window) {
				a.tabSize = size
				a.saveSettings()
				a.refreshMenu()
			},
		})
	}
	return out
}

func (a *app) lineNumItems() []*mygo.MenuItem {
	radio := func(label string, on bool, fn func()) *mygo.MenuItem {
		return &mygo.MenuItem{Label: label, Type: mygo.MenuItemRadio, Checked: on,
			Click: func(*mygo.MenuItem, *mygo.Window) {
				fn()
				a.saveSettings()
				a.refreshMenu()
			}}
	}
	return []*mygo.MenuItem{
		radio("绝对(&A)", a.lineNumbers && !a.relativeNums, func() { a.lineNumbers, a.relativeNums = true, false }),
		radio("相对(&R)", a.lineNumbers && a.relativeNums, func() { a.lineNumbers, a.relativeNums = true, true }),
		radio("关闭(&O)", !a.lineNumbers, func() { a.lineNumbers = false }),
	}
}

func (a *app) cursorItems() []*mygo.MenuItem {
	shape := func(label string, s editor.CursorShape) *mygo.MenuItem {
		return &mygo.MenuItem{Label: label, Type: mygo.MenuItemRadio, Checked: a.cursorShape == s,
			Click: func(*mygo.MenuItem, *mygo.Window) {
				a.cursorShape = s
				a.saveSettings()
				a.refreshMenu()
			}}
	}
	return []*mygo.MenuItem{
		shape("竖线(&B)", editor.CursorBar),
		shape("方块(&K)", editor.CursorBlock),
		shape("下划线(&U)", editor.CursorUnderline),
	}
}

func (a *app) languageItems() []*mygo.MenuItem {
	var out []*mygo.MenuItem
	current := a.buf.Language()
	plain := &mygo.MenuItem{Label: "纯文本(&T)", Type: mygo.MenuItemRadio, Checked: current == nil,
		Click: func(*mygo.MenuItem, *mygo.Window) {
			a.buf.SetLanguage(nil)
			a.refreshMenu()
		}}
	out = append(out, plain, mygo.Separator())
	for _, l := range editor.Languages() {
		out = append(out, &mygo.MenuItem{
			Label: l.Name, Type: mygo.MenuItemRadio, Checked: current == l,
			Click: func(*mygo.MenuItem, *mygo.Window) {
				a.buf.SetLanguage(l)
				a.refreshMenu()
			},
		})
	}
	return out
}

func (a *app) newFile() { a.confirmDiscard(a.newDocument) }

func (a *app) newDocument() {
	a.buf.Reset("")
	a.buf.SetLanguage(nil)
	a.base.Reset("")
	a.path = ""
	a.setEnding(endingCRLF)
	a.setStamp(fileStamp{})
	a.conflict = false
	a.relockFile("")
	a.ed.SetSelections(editor.Selection{})
	a.trackHeavy()
	a.updateTitle()
}

func (a *app) copyText(text string) {
	if text == "" {
		return
	}
	mygo.Clipboard.WriteText(text)
}

func (a *app) insert(text string) {
	a.ed.Insert(text)
	a.redraw()
}

func (a *app) timeFormat() string { return "2006-01-02 15:04:05" }

func (a *app) transform(fn func(string) string) {
	sels := a.ed.Selections()
	if len(sels) != 1 || sels[0].Anchor == sels[0].Head {
		a.notify(notice{appName, "请先选定要转换的文本。"})
		return
	}
	a.ed.Insert(fn(a.ed.SelectedText()))
	a.redraw()
}

func (a *app) clipPath(form string) string {
	switch form {
	case "name":
		return a.baseName()
	case "dir":
		return a.dirName()
	default:
		return a.path
	}
}

func (a *app) baseName() string {
	if a.path == "" {
		return "未命名"
	}
	return filepath.Base(a.path)
}

func (a *app) dirName() string {
	if a.path == "" {
		return ""
	}
	return filepath.Dir(a.path)
}

func (a *app) clearHistory() {
	ending := a.documentEnding()
	text := a.buf.String()
	a.buf.Reset(text)
	a.buf.SetLineEnding(ending)
	a.syncBase(text)
	a.trackHeavy()
	a.updateTitle()
	a.redraw()
}

func (a *app) tabsToSpaces() {
	lines := a.buf.LineCount()
	var edits []editor.Edit
	offset := 0
	for i := range lines {
		line := a.buf.Line(i)
		trimmed := strings.TrimLeft(line, "\t")
		if len(trimmed) != len(line) {
			spaces := strings.Repeat(" ", (len(line)-len(trimmed))*a.tabSize)
			edits = append(edits, editor.Edit{Start: offset, End: offset + len(line) - len(trimmed), Text: spaces})
		}
		offset += len(line) + 1
	}
	if len(edits) == 0 {
		return
	}
	a.buf.Edit(edits...)
	a.redraw()
}

func (a *app) heavyDoc() bool {
	return a.buf.Len() >= heavyDocBytes || a.buf.LineCount() > heavyDocLines
}

func (a *app) setHeavy(on bool) {
	if on == a.heavy {
		return
	}
	a.heavy = on
	if on {
		a.pending = notice{appName, fmt.Sprintf("文件超过 %d MB 或 %d 行，已进入大文件模式。", heavyDocBytes>>20, heavyDocLines)}
	}
}

func (a *app) applyLanguage(path string) {
	if a.heavyDoc() {
		a.buf.SetLanguage(nil)
		return
	}
	a.buf.SetLanguage(editor.LanguageFor(path))
}

func (a *app) syncBase(text string) {
	if a.heavyDoc() {
		a.base.Reset("")
		return
	}
	a.base.Reset(text)
}

func (a *app) trackHeavy() {
	heavy := a.heavyDoc()
	if heavy == a.heavy {
		return
	}
	a.setHeavy(heavy)
	if heavy {
		a.buf.SetLanguage(nil)
		a.base.Reset("")
	} else {
		a.applyLanguage(a.path)
		a.base.Reset(a.buf.String())
	}
	a.setMenu()
}

func (a *app) documentCharset() charset {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enc
}

func (a *app) documentEnding() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ending
}

func (a *app) setCharset(cs charset) {
	a.mu.Lock()
	a.enc = cs
	a.mu.Unlock()
	a.refreshMenu()
}

func (a *app) setEnding(ending string) {
	a.mu.Lock()
	a.ending = ending
	a.mu.Unlock()
	a.buf.SetLineEnding(ending)
	a.refreshMenu()
}

func (a *app) documentText() string {
	return applyEnding(a.buf.String(), a.documentEnding())
}

func (a *app) unifyLineEnding() {
	text := a.buf.String()
	unified := normalizeEndings(text)
	if unified == text {
		a.notify(notice{appName, "文件中的换行符均已统一为 " + endingName(a.documentEnding()) + "。"})
		return
	}
	a.buf.Set(unified)
	a.notify(notice{appName, "已将换行符统一为 " + endingName(a.documentEnding()) + "。"})
	a.redraw()
}

func (a *app) openFile() {
	a.confirmDiscard(func() {
		go func() {
			paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: a.window(), Title: "打开", Filters: fileFilters()})
			if err != nil || len(paths) == 0 {
				return
			}
			a.openPath(paths[0])
		}()
	})
}

func (a *app) openPath(path string) {
	go func() {
		data, stamp, err := a.readFile(path)
		if err != nil {
			a.notify(notice{"打开失败", err.Error()})
			return
		}
		a.onMain(func() { a.load(path, data, stamp) })
	}()
}

func (a *app) readFile(path string) ([]byte, fileStamp, error) {
	a.ioMu.Lock()
	defer a.ioMu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return nil, fileStamp{}, err
	}
	defer func(f *os.File) { _ = f.Close() }(f)
	info, err := f.Stat()
	if err != nil {
		return nil, fileStamp{}, err
	}
	data := make([]byte, info.Size())
	n, err := io.ReadFull(f, data)
	switch {
	case err == nil:
		rest, err := io.ReadAll(f)
		if err != nil {
			return nil, fileStamp{}, err
		}
		return append(data, rest...), stampOf(info), nil
	case err == io.EOF, errors.Is(err, io.ErrUnexpectedEOF):
		return data[:n], stampOf(info), nil
	default:
		return nil, fileStamp{}, err
	}
}

func (a *app) load(path string, data []byte, stamp fileStamp) {
	a.loadEncoded(path, data, stamp, detectCharset(data))
}

func (a *app) loadEncoded(path string, data []byte, stamp fileStamp, cs charset) {
	raw, ok := decodeText(cs, data)
	if !ok {
		a.notify(notice{"解码失败", "无法按 " + cs.name + " 解码文件，已按原文显示。"})
	}
	raw = strings.TrimPrefix(raw, "\ufeff")
	ending := detectEnding(raw)
	text := normalizeEndings(raw)
	a.mu.Lock()
	a.enc, a.ending = cs, ending
	a.mu.Unlock()
	a.buf.Reset(text)
	a.applyLanguage(path)
	a.buf.SetLineEnding(ending)
	a.syncBase(text)
	a.path = path
	a.setStamp(stamp)
	a.conflict = false
	a.relockFile(path)
	a.ed.SetSelections(editor.Selection{})
	a.trackHeavy()
	a.updateTitle()
	a.setMenu()
}

func (a *app) reload() {
	if a.path == "" {
		return
	}
	path := a.path
	a.confirmDiscard(func() { a.openPath(path) })
}

func (a *app) reloadCharsetAs(cs charset) {
	if a.path == "" {
		a.setCharset(cs)
		a.notify(notice{appName, "文件尚未保存，所选编码将在保存时生效。"})
		return
	}
	path := a.path
	a.confirmDiscard(func() { a.loadCharsetAs(path, cs) })
}

func (a *app) loadCharsetAs(path string, cs charset) {
	go func() {
		data, stamp, err := a.readFile(path)
		if err != nil {
			a.notify(notice{"打开失败", err.Error()})
			return
		}
		a.onMain(func() { a.loadEncoded(path, data, stamp, cs) })
	}()
}

func (a *app) saveCharsetAs(cs charset) {
	a.setCharset(cs)
	a.saveFile()
}

func (a *app) quit() {
	a.confirmDiscard(func() {
		a.quitting = true
		mygo.App.Quit()
	})
}

func (a *app) saveFile() {
	if a.path == "" {
		a.saveFileAs()
		return
	}
	path := a.path
	if readOnlyFile(path) {
		a.saveAsThen("另存为（文件为只读）", nil)
		return
	}
	a.saveTo(path, nil)
}

func (a *app) saveAsThen(title string, then func()) {
	def := a.defaultPath()
	go func() {
		path, err := mygo.Dialog.Save(mygo.SaveDialogOptions{Parent: a.window(), Title: title, DefaultPath: def, Filters: fileFilters()})
		if err != nil || path == "" {
			return
		}
		stamp, werr := a.writeTo(path)
		a.onMain(func() {
			if werr != nil {
				a.notify(notice{"保存失败", werr.Error()})
				return
			}
			a.afterSave(path, stamp)
			if then != nil {
				then()
			}
		})
	}()
}

func (a *app) saveFileAs() { a.saveAsThen("另存为", nil) }

func (a *app) saveTo(path string, then func()) {
	go func() {
		switch a.planSave(path) {
		case saveAsk:
			a.askConflict(path, then)
			return
		case saveReload:
			return
		default:
		}
		a.commitSave(path, then)
	}()
}

func (a *app) planSave(path string) savePlan {
	a.ioMu.Lock()
	stamp, ok, err := statStamp(path)
	a.ioMu.Unlock()
	if err != nil || !ok || !a.diskChanged(stamp) {
		return saveProceed
	}
	if !a.buf.Modified() {
		a.openPath(path)
		return saveReload
	}
	a.onMain(a.markConflict)
	return saveAsk
}

func (a *app) askConflict(path string, then func()) {
	a.ask(askSpec{
		title:   appName,
		detail:  "文件已被其他程序修改。\n" + filepath.Base(path) + "\n覆盖保存会丢弃其他程序所做的更改。",
		buttons: []string{"覆盖保存", "重新载入", "取消"},
		answer: func(i int) {
			switch i {
			case 0:
				a.commitSave(path, then)
			case 1:
				a.openPath(path)
			}
		},
	})
}

func (a *app) commitSave(path string, then func()) {
	go func() {
		stamp, err := a.writeTo(path)
		a.onMain(func() {
			if err != nil {
				a.notify(notice{"保存失败", err.Error()})
				return
			}
			a.afterSave(path, stamp)
			if then != nil {
				then()
			}
		})
	}()
}

func (a *app) afterSave(path string, stamp fileStamp) {
	a.buf.MarkSaved()
	if a.heavyDoc() {
		a.base.Reset("")
	} else {
		a.base.Reset(a.buf.String())
	}
	a.path = path
	a.setStamp(stamp)
	a.conflict = false
	a.applyLanguage(path)
	a.relockFile(path)
	a.updateTitle()
}

func (a *app) confirmDiscard(fn func()) {
	if !a.buf.Modified() {
		fn()
		return
	}
	a.ask(askSpec{
		title:   appName,
		detail:  "是否保存对下列文件的更改？\n" + a.windowTitle(),
		buttons: []string{"保存", "不保存", "取消"},
		answer: func(i int) {
			switch i {
			case 0:
				a.saveAndRun(fn)
			case 1:
				fn()
			}
		},
	})
}

func (a *app) saveAndRun(fn func()) {
	path := a.path
	if path == "" {
		a.saveAsThen("另存为", fn)
		return
	}
	a.saveTo(path, fn)
}

func (a *app) defaultPath() string {
	if a.path != "" {
		return a.path
	}
	return "未命名.txt"
}

func (a *app) writeTo(path string) (fileStamp, error) {
	a.ioMu.Lock()
	defer a.ioMu.Unlock()
	err := writeFileSafely(path, func(w io.Writer) error {
		cs := a.documentCharset()
		data, lost := encodeText(cs, a.documentText())
		if _, err := w.Write(data); err != nil {
			return err
		}
		if lost > 0 {
			a.notify(notice{appName, fmt.Sprintf("有 %d 个字符无法用 %s 表示，已保存为“?”。", lost, cs.name)})
		}
		return nil
	})
	if err != nil {
		return fileStamp{}, err
	}
	stamp, _, _ := statStamp(path)
	return stamp, nil
}

func writeFileSafely(path string, write func(io.Writer) error) error {
	if realPath, err := filepath.EvalSymlinks(path); err == nil {
		path = realPath
	}
	info, statErr := os.Stat(path)
	tmp, err := createSibling(path)
	if err != nil {
		if statErr == nil && errors.Is(err, fs.ErrPermission) {
			return writeFileInPlace(path, write)
		}
		return err
	}
	if err := writeAndSync(tmp, write); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if statErr == nil {
		_ = os.Chmod(tmp.Name(), info.Mode())
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		if statErr == nil {
			return writeFileInPlace(path, write)
		}
		return err
	}
	return nil
}

func createSibling(path string) (*os.File, error) {
	dir, base := filepath.Split(path)
	for range 10 {
		f, err := os.OpenFile(filepath.Join(dir, "."+base+"."+uuid.NewString()[:8]+".tmp"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
	return nil, fmt.Errorf("无法在 %s 旁边创建临时文件", filepath.Dir(path))
}

func writeAndSync(f *os.File, write func(io.Writer) error) error {
	if err := write(f); err != nil {
		return err
	}
	return f.Sync()
}

func writeFileInPlace(path string, write func(io.Writer) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if err := writeAndSync(f, write); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func fileFilters() []mygo.FileFilter {
	return []mygo.FileFilter{
		{Name: "文本文件", Extensions: []string{"txt", "log", "ini", "csv", "md", "json", "xml", "go", "js", "ts"}},
		{Name: "所有文件", Extensions: []string{"*"}},
	}
}

func stampOf(info os.FileInfo) fileStamp {
	return fileStamp{modTime: info.ModTime().UnixNano(), size: info.Size()}
}

func statStamp(path string) (fileStamp, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileStamp{}, false, nil
	}
	if err != nil {
		return fileStamp{}, false, err
	}
	return stampOf(info), true, nil
}

func readOnlyFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0o200 == 0
}

func (a *app) setStamp(s fileStamp) {
	a.mu.Lock()
	a.savedStamp = s
	a.mu.Unlock()
}

func (a *app) diskChanged(stamp fileStamp) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.savedStamp != (fileStamp{}) && a.savedStamp != stamp
}

func (a *app) checkDisk() {
	if a.path == "" {
		return
	}
	path := a.path
	go func() {
		a.ioMu.Lock()
		stamp, ok, err := statStamp(path)
		a.ioMu.Unlock()
		if err != nil || !ok {
			return
		}
		if !a.diskChanged(stamp) {
			a.onMain(func() {
				if a.conflict {
					a.clearConflict()
				}
			})
			return
		}
		if a.buf.Modified() {
			a.onMain(a.markConflict)
			return
		}
		a.openPath(path)
	}()
}

func (a *app) markConflict() { a.conflict = true }

func (a *app) clearConflict() { a.conflict = false }

func normalizePath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func fileSocketPath(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return filepath.Join(os.TempDir(), "my-notepad-go-"+hex.EncodeToString(sum[:8])+".sock")
}

func fileKey(path string) string {
	if realPath, err := filepath.EvalSymlinks(path); err == nil {
		path = realPath
	}
	return normalizePath(path)
}

func spawnInstances(files []string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	for _, f := range files {
		cmd := exec.Command(exe, f)
		if cmd.Start() == nil {
			go func() { _ = cmd.Wait() }()
		}
	}
}

func (a *app) lockPath(normalized string, ping bool) bool {
	if normalized == "" {
		return false
	}
	sock := fileSocketPath(normalized)
	for attempt := range 5 {
		if conn, err := net.DialTimeout("unix", sock, time.Second); err == nil {
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if ping {
				_, _ = conn.Write([]byte{0})
				_, _ = io.Copy(io.Discard, conn)
			}
			_ = conn.Close()
			return true
		}
		_ = os.Remove(sock)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
			continue
		}
		if ul, ok := ln.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false)
		}
		fi, _ := os.Stat(sock)
		a.mu.Lock()
		a.filePath, a.fileLn, a.fileSock, a.fileFi = normalized, ln, sock, fi
		a.mu.Unlock()
		go a.serveFileLock(ln)
		return false
	}
	return false
}

func (a *app) releaseFileLock() {
	a.mu.Lock()
	ln, sock, fi := a.fileLn, a.fileSock, a.fileFi
	a.fileLn, a.fileSock, a.fileFi, a.filePath = nil, "", nil, ""
	a.mu.Unlock()
	if ln == nil {
		return
	}
	_ = ln.Close()
	if fi != nil {
		if cur, err := os.Stat(sock); err == nil && os.SameFile(cur, fi) {
			_ = os.Remove(sock)
		}
	}
}

func (a *app) relockFile(path string) {
	key := ""
	if path != "" {
		key = fileKey(path)
	}
	a.mu.Lock()
	same := key != "" && a.filePath == key
	a.mu.Unlock()
	if same {
		return
	}
	a.releaseFileLock()
	a.lockPath(key, false)
}

func (a *app) serveFileLock(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
		a.focusWindow()
	}
}

func (a *app) focusWindow() {
	w := a.window()
	if w == nil {
		return
	}
	if w.IsMinimized() {
		w.Restore()
	}
	w.Show()
	w.Focus()
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func mergeSplitPaths(args []string) []string {
	if n := len(args); n <= 1 || n > 32 {
		return args
	}
	out := make([]string, 0, len(args))
	var b strings.Builder
	for i := 0; i < len(args); i++ {
		if isFile(args[i]) {
			out = append(out, args[i])
			continue
		}
		b.Reset()
		b.WriteString(args[i])
		merged := false
		for j := i + 1; j < len(args); j++ {
			b.WriteByte(' ')
			b.WriteString(args[j])
			if isFile(b.String()) {
				out = append(out, b.String())
				i = j
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, args[i])
		}
	}
	return out
}

func parseArgs(argv []string) launch {
	var l launch
	if len(argv) > 0 && argv[0] == "/z" {
		argv = argv[1:]
	}
	if len(argv) > 0 && strings.EqualFold(filepath.Base(argv[0]), "notepad.exe") {
		argv = argv[1:]
	}
	l.files = mergeSplitPaths(argv)
	return l
}

func main() {
	mygo.App.SetName(appName)
	l := parseArgs(os.Args[1:])
	a := newApp()
	if len(l.files) > 1 {
		spawnInstances(l.files[1:])
	}
	if len(l.files) > 0 && a.lockPath(fileKey(l.files[0]), true) {
		return
	}
	mygo.App.OnQuit(a.releaseFileLock)
	mygo.App.WhenReady(func() {
		w := mygo.NewWindow(mygo.WindowOptions{
			Title:     a.windowTitle(),
			Width:     1000,
			Height:    720,
			MinWidth:  480,
			MinHeight: 320,
			StateKey:  "main",
			Content:   ui.View(a.view),
		})
		a.setWindow(w)
		menu := a.buildMenu()
		if runtime.GOOS == "darwin" {
			mygo.App.SetMenu(menu)
		} else {
			w.SetMenu(menu)
		}
		w.OnClose(func(e *mygo.CloseEvent) {
			if a.quitting || !a.buf.Modified() {
				return
			}
			e.PreventDefault()
			a.confirmDiscard(func() {
				a.quitting = true
				w.Close()
			})
		})
		w.OnFocus(a.checkDisk)
		if len(l.files) > 0 {
			a.openPath(l.files[0])
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

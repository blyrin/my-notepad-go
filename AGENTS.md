# 仓库指南

My Notepad Go 是基于 [MyGo](https://github.com/egoist/mygo) 原生 UI 的 Windows 记事本替代品。**单个 `main` 包、纯 Go、无 cgo、无前端资源**：没有 Vite/TypeScript/bridge，UI 全部由 MyGo `ui` 包绘制，`resources/` 只放应用图标。不要引入 webview、IPC 或 TypeScript 客户端。

## 项目结构与模块组织

扁平单包，无子目录源码：

| 路径 | 职责 |
| --- | --- |
| `main.go` | 应用主体：`app` 状态与设置持久化、字符编码表（`charset` / `charsetGroups`）与检测编解码、行尾符常量与转换、`view()` 视图构建、`buildMenu()` 菜单树、文件锁、原子写入与磁盘冲突检测、`main()` 入口 |
| `platform_windows.go` | `//go:build windows`：注册表读写，实现「劫持系统记事本」与「右键菜单」两个开关 |
| `platform_other.go` | `//go:build !windows`：上述两族功能的同签名空实现 |
| `mygo.json` | 应用元数据；改 `identifier` 会影响用户数据目录与窗口状态键，需谨慎 |
| `.agents/skills/mygo-maintenance/SKILL.md` | MyGo UI 维护规范，改 UI 前必读 |

`my-notepad-go.exe`、`build/`、`.mygo/`、`mygo_windows_*.syso` 是产物，已在 `.gitignore`，不要手动编辑或提交。

### 平台成对规则

涉及系统能力的功能必须成对提供，漏掉任一侧会破坏跨平台构建：

- `platform_windows.go` — 真实实现，用 `syscall.NewLazyDLL` 或 `golang.org/x/sys/windows/registry`，**禁止 `import "C"`**；
- `platform_other.go` — 相同签名的空实现（返回 `false`/`nil`/`""`，错误返回「当前平台不支持」），保证 `linux`、`darwin`、`js` 仍可编译。

## 核心业务工作流

### 启动与命令行劫持

`main()` 是唯一入口，流程自上而下：

1. `parseArgs` 解析 `/z`、`notepad.exe` 参数（劫持系统记事本的关键路径，重构不得破坏），其余参数一律按文件路径收集。Windows 用 IFEO 劫持时只给映像路径加引号、文件参数原样拼接，带空格的路径会被拆成多个参数，因此 `launchFiles` 会按「磁盘上是否存在」把连续参数贪心拼回最长路径；拼不回来时保持逐参数原样（与真实记事本一致）。
2. `newApp()` 创建状态；多文件时 `spawnInstances` 把第 2 个起的文件交给新进程。
3. `a.lockPath(fileKey(...))` 抢单实例锁（`%TEMP%` 下 unix socket）：抢不到说明同文件已有窗口，Accept 侧把已有窗口还原聚焦后退出。
4. `WhenReady` 里建窗口、`buildMenu()` 挂菜单（darwin 用 `App.SetMenu`）、注册 `OnClose` / `OnFocus`，最后 `a.openPath(l.files[0])` 载入首个文件。

### 打开文档

`openFile()`（菜单/拖拽 `openDropped` 同入口）→ 选文件 → goroutine 读盘 → `detectCharset` 判定编码 → `decodeText` 解码 → `loadEncoded` 写入 `a.buf` 并 `setCharset` / `setEnding` 记录文档属性、`setStamp` 记录磁盘指纹。

打开链路按大文件设计，改动时保持：`readFile` 按 `Stat` 大小预分配（文件在读盘期间变大时再续读，不要回退到 `io.ReadAll` 反复扩容）；`decodeText` 对「已是 UTF-8 且 `utf8.Valid`」走 `string(data)` 快路径，不再过 transform；`normalizeEndings` 单遍扫描 `\r` 一次成型（CRLF 文件不要做两次 `ReplaceAll`，那会多出两份全文本）。实测 200MB UTF-8 文件约 0.7s、GC 后存活堆约 3 倍文件大小（`a.buf` rope + `a.base` 快照 + 编辑器态），瓶颈已转移到 `buf.Reset` / `base.Reset` 的 rope 构建（占六成以上）。

### 保存文档

`confirmDiscard` 是所有可能丢数据的入口（新建、打开、从文件还原、退出、`OnClose`）的统一闸门，弹「保存/不保存/取消」；确认保存走 `commitSave(path, then)`，`then` 回调串联「保存后继续做某件事」。`writeTo` 负责合成字节：`documentText()`（`applyEnding` 套行尾符）+ `encodeText` 编码后交 `writeFileSafely` 落盘。

**保存必须走 `writeFileSafely`**（旁边隐藏临时文件 + `Sync` + 保留权限位 + `Rename` 替换），不要回到 `os.Create` 原地截断；临时文件建不了（目录不可写）或替换失败（目标被占用）时才回退 `writeFileInPlace`。只读文件不阻止保存，而是引导「另存为（文件为只读）」。`encodeText` 遇到目标编码无法表示的字符时逐段跳过并补 `?`，返回丢失数量由 toast 告知，不要因编码失败丢弃后半篇文档。

### 编码与换行单向流水线

内存文本永远是「UTF-8 + `\n`」：`a.buf` 只存规范化后的文本，`a.enc` 记文档编码、`a.ending` 记行尾符，**只在落盘时合成字节、读入时拆解**。改这里必须保持这条单向流水线，不要把编码/行尾符写回 `a.buf`。

编码检测顺序固定：BOM → 合法 UTF-8 → `detectLegacyCharset`。最后一步只做一次 GB18030 无损解码试探：能解出（不含 `utf8.RuneError`）就按 GB18030 读，否则退回 ISO-8859-1；**试探前必须先剥掉样本末尾的一个 `utf8.RuneError`**，因为 64KB 抽样（`charsetSample`）大概率把多字节字符切断，解码器对残缺尾序列也会吐 `utf8.RuneError`，不剥就会把大文件中文 Legacy 文本误判成 ISO-8859-1 乱码（约一半概率，取决于截断对齐）。GB18030 覆盖全部 Unicode 且是 GBK 超集，中文 Legacy 文本走这条路；日、韩、繁中等非中文 CJK 的 Legacy 文件可能被按 GB18030 误读成乱码，此时用「编码 → 重新载入为...」手工纠正，不要为了区分它们把启发式打分加回来。检测只用前 64KB（`charsetSample`）。

「编码」子菜单只平铺常用五项（UTF-8 / UTF-8 带 BOM / UTF-16 LE / UTF-16 BE / GB18030），由 `charsetMenuIDs` 决定；其余编码经「更多编码...」弹窗选择，表 `charsetGroups` 仍是唯一编码来源。`decodeText` 返回 `(string, ok)`：解码失败回退原文并由 `loadEncoded` toast 告知，不要静默吞掉。

`a.buf.LineEnding()` 与 `a.ending` 保持同步（`setEnding` 两处都写），但**权威值是 `a.ending`**——`editor.Buffer` 内部只区分 CRLF/LF，CR 只能由 `applyEnding` 在写出时生成。

### 磁盘冲突检测

「文件是否被别的程序改过」只认 `fileStamp`（修改时间 + 大小）指纹比对，不要用单一字段猜；取不到指纹时按「没有改过」处理——宁可漏报也不误报。窗口聚焦时 `OnFocus` 触发 `checkDisk`：文档没改过就重新载入，改过了置 `conflict` 由状态栏提示。

### 大文件模式

文档达到 `heavyDocBytes`（50MB）或行数超过 `heavyDocLines`（200 万）即进入大文件模式，判定走 `heavyDoc()`（`buf.Len()` / `buf.LineCount()`，均为 O(1)，可每帧现算，不要缓存成会过期的状态）。`a.heavy` 是边沿状态，由 `trackHeavy()` 统一维护：`view()` 每帧先跑一次，载入路径（`loadEncoded` / `newDocument` / `clearHistory`）也调它；编辑跨越阈值时同步摘除/恢复语言、清空/重建 `a.base`（退出大文件模式时 base 按当前文本重建，变更标记从零起算）并重挂菜单、弹 toast。

大文件模式下**强制关闭且禁止用户开启**：语法高亮（`applyLanguage` 不设语言）、括号自动闭合（`NoAutoClose`）、代码折叠（`NoFolding`）、变更标记（`DiffBase` 置 nil）、自动换行（`SoftWrap` 置 false），并且不建 `a.base` 快照（`syncBase`，省一份等长 rope）。`afterSave` / `trackHeavy` 里对大文档不要无条件 `buf.String()` 再丢弃——快照文本要么复用现成变量（`syncBase(text)`），要么先过 `heavyDoc()` 分支。菜单侧用 `heavyCheck` / `disableIf` 置灰「自动换行 / 代码折叠 / 折叠子菜单 / 变更记录标记子菜单 / 语法高亮方案集 / 自动关闭括号与引号」，快捷键 `Ctrl+W` 等经 `toggleSetting` 拦截并提示，状态栏追加「大文件模式」窗格。

取舍依据（MyGo 0.5.5 实测）：200MB 代码文件全量 tokenize 约 2.7s 后台 CPU + 442MB 分配；`DiffBase` 每次打开都全量 diff 两份文本，约 227ms + 552MB 瞬时分配；自动换行的 `measureWraps` 同样要走遍全文档。括号匹配本身有 1000 行扫描上限且按「版本+光标」缓存，无语言时不做 token 化，成本可接受，MyGo 也没有单独开关，故只关自动闭合、不假装关掉了匹配。调整阈值或增删强制关闭项时，同步更新本段与 `.local/bench_open_test.go` 里的 `TestHeavyDocMode` / `TestHeavyDocLines`。

## 构建与开发命令

命令均在仓库根目录执行，`mygo` 由 `go.mod` 的 `tool` 指令提供：

```sh
go run .                              # 开发方式启动
go tool mygo dev                      # 监视 Go 代码变更自动重建重启，保留窗口状态
go tool mygo build                    # 生产构建到 mygo.json 的 out（build/），Windows 需 NSIS
go tool mygo vet .                    # go vet + 原生 UI 构建期生命周期检查，提交前运行
go tool mygo migrate-ui .             # 预览 UI 值 API 迁移（-write 才落盘）
```

- 验证以静态阅读与 `go tool mygo vet .` 为准；不执行 `go build`/`go run`/`mygo build`/`mygo dev`，不编写与执行测试。
- 依赖：`golang.org/x/text`（编解码）与 `github.com/google/uuid`（临时文件名、GUID）。新增编码只改 `main.go` 的编码表 `charsetGroups` 与菜单可见集 `charsetMenuIDs`，不引新库。
- 动 `platform_*.go` 后，用 `GOOS=linux go build ./...` 与 `GOOS=js GOARCH=wasm go build ./...` 交叉验证。
- 升级 MyGo 时，按 `.agents/skills/mygo-maintenance/SKILL.md` 阅读 `docs/ui/migration.md`（`0.x` 小版本升级也算破坏性升级）。

## 编码风格

- `gofmt` 默认；接收者统一 `a *app`；菜单构造用小写 helper `item` / `check` / `command` / `sub`，平台常量集中在文件顶部。
- **面向用户的字符串一律中文**（菜单、按钮、应用内提示与询问、错误信息），标识符用英文；菜单标签用 `(&X)` 标记访问键，与 Windows 记事本一致。
- **不写注释**（`//go:build` 除外），靠命名自解释；行为规范只记录在本文档，不散落进源码。
- 优先 Go 1.27+ 内建能力（`for i := range n`、`max`/`min`、`strings.SplitSeq`、`slices.Contains`）；错误用 `fmt.Errorf` 包中文上下文。

## 配置与环境

- `mygo.json`：应用元数据，`out` 指向 `build/`；`identifier` 决定用户数据目录与窗口状态键，改动会影响老用户数据定位。
- 用户设置：`%APPDATA%\My Notepad Go\settings.json`，由 `loadSettings` / `saveSettings` / `captureSettings` / `applySettings` 管理，**不得提交**。
- 系统集成开关（劫持记事本、右键菜单）：注册表残留值指向已不存在的可执行文件时视为陈旧（`debuggerStale` / `pathStale`），允许覆盖与清除，避免程序移动位置后无法解除劫持。
- 本地临时产物一律放 `.local/`，保持仓库根目录干净；`.gitignore` 已排除 `build/`、`.mygo/`、`mygo_windows_*.syso`、可执行文件与 IDE 目录。
- 新增可配置字段必须同步四处：`app` 字段、`settings` 结构体与 JSON tag、`captureSettings` / `applySettings`、`settings()` 中到 `editor.State` 的映射。`applySettings` 对取值范围做防御（字号 8–40、字重 100–900、制表符宽度仅 2/4/6/8）。

## 架构概览

以下规则无法从代码一眼看出，改动前必读。

### 线程与锁

- 慢 I/O（文件、对话框、剪贴板）放 goroutine；**应用状态修改必须经 `a.onMain` 回主线程**；`a.redraw()`（`Invalidate`）只请求重绘，不作为同步写模型的手段。
- `a.onMain` 是 `Window.Update` 的唯一入口：内部经加锁的 `a.window()` 取窗口并防空，禁止裸写 `a.win.Update` / `a.win.SetTitle` / `Parent: a.win`。`refreshMenu` 走 `a.onMain(a.setMenu)`；已在主线程（`Update` 闭包内）的调用点直接用 `a.setMenu()`，不嵌套 `Update`。
- `Update` / `refreshMenu` 内部已申请一帧，其后不要再接 `redraw()`；`markConflict` / `clearConflict` 只置 `conflict` 标志，帧由外层 `Update` 提供。
- `a.ioMu` 串行化文档文件的读、写、`statStamp` 磁盘检查：各自短持锁，不嵌套 `a.mu`，不跨 `win.Update` 持锁。`a.savedStamp` 由 `a.mu` 保护，经 `setStamp` / `diskChanged` 访问。`a.enc` / `a.ending` 是文档属性（不持久化），同样由 `a.mu` 保护，经 `documentCharset` / `documentEnding` 读、`setCharset` / `setEnding` 写；保存在 goroutine 里读它们。`a.path` / `a.conflict` 等裸字段没有锁：goroutine 里要用，必须在主线程先取好值再进 goroutine（如 `saveAsThen` 的默认路径），或把读取挪进 `a.onMain` 闭包（如 `checkDisk` 清除冲突标志）。`editor.Buffer` 的方法自身线程安全，可在 goroutine 直接调。

### 编辑器三层数据

- `a.buf` — 文档内容、撤销栈、语言、行尾符；`a.base` — 上次保存时的文本快照，仅作 `DiffBase`，支撑变更标记、上一处/下一处更改、还原此更改；`a.ed` — 视图态（字体、光标、选区）。
- `setFontSize` / `setFontWeight` / `setFontFamily` 经 `applyFont()` 直接改 `a.ed.Font` 后 `redraw()`，**不要重建 `editor.State`**——滚动位置、折叠、选区撤销栈存在 State 内部，重建即丢失。

### 视图与反馈

- 改 UI 前必读 `.agents/skills/mygo-maintenance/SKILL.md`。核心：视图可能因单个事件重建多次，**绝不缓存 `ui.Element` 或其子部件**，只保存应用数据与 `ui.Handle`；副作用放 `OnClick`/`OnShortcut`；绘制回调只做绘制。
- `notify` / `ask` 只写待办槽位（`a.pending` / `a.asking`），由视图消费为 toast 或 `AlertDialog` 派发 `answer`；不要在视图构建过程中直接关窗或退出。
- 不用系统消息框（`mygo.Dialog.Message`）；唯一例外是文件选择，用 `Dialog.Open` / `Dialog.Save`。
- 失效请求统一走 `a.redraw()`（即 `Window.Invalidate`）：`win.Update` 与 `refreshMenu` 内部已 `contentChanged()` 申请一帧，**其后不要再接 `redraw()`**，否则同一事件重建两帧视图；`SetMenu` 自身不带帧，只改菜单状态时用它即可。反过来「该刷不刷」也是缺陷——在 `view()` 之外置位弹窗类状态（`a.fontPicker` / `a.encPicker`）必须补 `a.redraw()`，原生菜单点击不自带一帧；而在 `view()` 内关闭弹窗（`= false`）不必刷，同一帧后续就会读到。
- 状态栏的选区统计走 `Snapshot.Chunks` 流式计数并按「buffer 版本 + 选区」缓存（`a.selCache`），不要对选区 `Slice` 出整段字符串——大文档全选时那是每帧一份全文拷贝。

### 菜单与快捷键

- **菜单是唯一权威的选项状态源**：勾选态每次 `buildMenu()` 现场读 `app` 字段，改选项后 `refreshMenu()`（必要时 `saveSettings()`），不要就地改菜单项。
- 快捷键双轨：菜单用 `Accelerator`，代码内用 `c.OnShortcut` / `a.ed.OnInput`。
- 字体（族 / 字号 / 字重）只在「外观 → 字体...」对话框里设置，菜单不放缩放与字重项；字号另有快捷键 `Ctrl+加号` / `Ctrl+减号` / `Ctrl+0`（重置）与 `Ctrl+滚轮`。

## 项目专属 Agent 说明

面向 AI 编码代理与自动化工具的项目约束：

- 改 UI 前先读 `.agents/skills/mygo-maintenance/SKILL.md`，遵循其生命周期、身份与线程规范。
- **不执行** `go build` / `go run` / `mygo build` / `mygo dev`，不编写、不执行测试；验证靠静态阅读加 `go tool mygo vet .`。
- 动 `platform_*.go` 后必须交叉验证 `GOOS=linux` 与 `GOOS=js GOARCH=wasm`。
- 临时文件放 `.local/`；大范围删除文件前先 dry run。
- 输出遵循本文档全部规范：中文用户字符串、不写注释、平台成对、保存走 `writeFileSafely`。

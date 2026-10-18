# assets/icon —— 图标的矢量源与 Windows 清单

图标是**构建期派生物**，仓库里不保留任何 `.ico` / `.png`。入库的只有三样：
矢量源（`app.svg`，给人看的规格）、清单（`app.manifest`，文本 XML）、这份说明。

真正被调用的代码在 `internal/icon`，构建期用 `tools/genicon` 落成 ICO。
**改图形改 `internal/icon/icon.go`**，并同步 `app.svg` 里的数字 —— 两边是
同一套几何的两种写法（代码是真身，SVG 是对照）。

## 派生链

```
internal/icon（Go：圆角渐变底板 + 白气泡 + 蓝四角星）
        │  go run ./tools/genicon -out .build/icon/app.ico
        ▼
.build/icon/app.ico（构建期临时产物，.gitignore 忽略）
        │  go run github.com/akavel/rsrc@v0.10.2 -ico … -manifest …
        ▼
cmd/desktop/rsrc.syso（PE 资源目标文件，.gitignore 忽略）
        │  链接器把它并进 .rsrc 段
        ▼
dist/NaiBox.exe（exe 图标 + DPI 清单都在里面）
```

托盘图标不走这条链：运行时直接 `icon.ICO(32)` 拿字节喂给 systray。

## 三条命令

```bash
# 1. 生成 ICO（多尺寸 16/32/48/64/128/256；加 -png 目录还会顺带落各尺寸 PNG 供人眼查）
go run ./tools/genicon -out .build/icon/app.ico -png .build/icon

# 2. ICO + manifest → 资源对象（放 cmd/desktop/ 才会被链接进 PE）
GOPROXY=https://goproxy.cn,direct go run github.com/akavel/rsrc@v0.10.2 \
    -arch amd64 -ico .build/icon/app.ico -manifest assets/icon/app.manifest \
    -o cmd/desktop/rsrc.syso

# 3. 验收：PE 是 GUI 子系统且资源目录非空（图标 + 清单真的进去了）
go run ./tools/genicon -verify dist/NaiBox.exe
```

第 3 条不是锦上添花：`go build` 成功**不**代表资源进了 exe —— `.syso` 没被
链接器收进去时编译器一声不响，双击得到的还是默认白框。`-verify` 读 PE 头回答
两件事：`Subsystem == 2`（双击无黑框）与资源目录非空。

完整的桌面壳构建（三步串起来 + 体积 + SHA256）用
[`scripts/build-desktop.ps1`](../../scripts/build-desktop.ps1)。

## 设计说明

图形是 **浅蓝渐变圆角底板 + 深蓝字母 N**（NaiBox 首字母），几何定义在
`internal/icon/icon.go` 的常量里，本目录的 `app.svg` 是同一套数字的矢量规格。

| 元素 | 取值 | 为什么这么定 |
| --- | --- | --- |
| 底板 | `3..61`，圆角 15，`#a8dcf7 → #6fbfef`（自上而下） | 留 3 单位内边距：16px 托盘里贴边的底板会被任务栏视觉裁掉一圈；浅蓝到中浅蓝的微渐变避免纯色在大尺寸下显得死板 |
| 字母 N | 左竖 `x=21` / 右竖 `x=43`，上 `y=16` 下 `y=48`，笔画直径 7.5，圆头 | 笔画占 64 栅格的 11.7%，到 16px 是 1.9 像素 —— 再细会在任务栏里断成一串点；横跨 22 让字面占满底板中间 2/3（图标字体常见的视觉居中比例） |
| N 的颜色 | `#0d4a75`（深蓝） | **不能用白色**：白字压在浅蓝底上对比度只有约 1.3:1（两者亮度太接近），16px 下糊成一片白斑；深蓝对浅蓝约 6.5:1，小尺寸仍一眼可辨 |

形状在 64×64 的设计栅格上定义（与 `app.svg` 的 viewBox 一致），按
`size/64` 缩放；每个尺寸都在 `4×4` 超采样下光栅化后取平均，所以 16px 是
"用 64px 的精度画的 16px"，不是从大图缩下来的。

N 用**三条带圆头的线段**（点到线段距离 ≤ 半径）实现，而不是多边形填充：
这样圆头、圆角、笔画宽度都能直接从"半径"一个参数得到，不必手工枚举拐角顶点。

## 托盘图标必须是 ICO，不能是 PNG

`energye/systray` 的 Windows 分支把字节写进 `%TEMP%\systray_temp_icon_<md5>`
（**无扩展名**），再 `LoadImageW(0, path, IMAGE_ICON, 0, 0, LR_LOADFROMFILE|LR_DEFAULTSIZE)`。
`LoadImage` 只按 ICO/CUR 的**内容**解析，不嗅探 PNG：喂 PNG 进去它会打一行
`systray error: unable to set icon` 就返回 —— 不 panic、不报错、托盘直接没图标。

所以托盘走 `icon.ICO(32)`：单尺寸调用一律输出 DIB 帧（32bpp BGRA + 自下而上
行序 + AND 掩码），这是 `LoadImage` 最没有商量余地的格式。`icon.PNG(n)` 只用于
构建期（ICO 里的大尺寸帧）与人工查验，**不是**托盘图标的交付路径。
这条也写在 `internal/icon` 的包注释里，`icon_test.go` 有专门的测试钉住。

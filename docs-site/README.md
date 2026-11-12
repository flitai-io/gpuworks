# 文档站本地预览

使用与 CI 一致的 Hugo extended 0.165.0，在仓库根目录运行：

```bash
HUGO_BIN=/path/to/hugo bash docs-site/preview.sh
```

打开 <http://127.0.0.1:1313/docs/how-to/>。首次运行会按 `build.sh` 获取 OINK 主题；本地已有的主题可放在 `docs-site/themes/oink/`。端口被占用时设置 `DOCS_PREVIEW_PORT`。

默认只允许本机访问。需要与同一局域网的同事共享时，设置 `DOCS_PREVIEW_BIND=0.0.0.0`，并将 `DOCS_PREVIEW_BASE_URL` 设为 `http://<本机局域网IP>:<端口>/docs/`，端口与 `DOCS_PREVIEW_PORT` 保持一致。同事使用该地址访问；本机换网或 IP 改变后，需要更新地址并重启预览。

操作指南的分类、导航标签和顺序来自 `docs/README.md` 的操作指南清单；用户教程的顺序来自同一文件的教程清单，目录页、侧栏和上一篇／下一篇保持一致。修改文档或清单后，在另一个终端运行 `python3 docs-site/stage_docs.py`，Hugo 会重新加载生成内容；不要手工修改 `docs-site/content/`。

侧栏使用统一文档树，可以从任何页面直接展开“操作指南 → 分类 → 文章”，也可切换到教程、参考等栏目。检查目录页的四组及文章顺序，展开侧栏各组，进入文章确认所属组展开、当前项高亮，并检查链接仍使用原来的文章地址。提取和暂存回归检查：

```bash
HUGO_BIN=/path/to/hugo python3 -m unittest discover -s docs-site -p 'test_*.py'
```

## 合并前构建检查

在 Git checkout 中运行完整生产构建，需要 Bash、Git、Python 3.9+ 和 Hugo extended 0.165.0：

```bash
HUGO_BIN=/path/to/hugo bash docs-site/build.sh
```

构建会执行站点测试、完整翻译检查和 Hugo 生产构建，并检查中英文首页与搜索索引。首次构建会从公开的 `gpuworks_website` 仓库下载 OINK 主题；访问不到 github.com 的环境用 `WEBSITE_REPO` 指向可访问的副本（内网 GitLab CI 用该变量中的只读部署令牌 URL），不要把令牌写入仓库。CI 的 `docs-build` 覆盖 `docs/`、`docs-en/`、`docs-site/` 和 CI 配置的变更。

提交时必须包含新增的英文稿、图片、下载文件、模板、脚本及翻译登记 JSON；只提交已跟踪文件的修改会漏掉这些构建输入。`content/`、`public/`、`resources/`、`themes/` 和 `static/assets/` 是生成或下载目录，不应提交。验证首次构建时使用独立 Git checkout，不复制这些目录或 Hugo 缓存。

## 中英文文档

中文源位于 `docs/`，英文译文位于 `docs-en/`，使用相同文件名与目录结构。暂存脚本生成 `.en.md`，
Hugo 将中文发布到 `/docs/`、英文发布到 `/docs/en/`，主题语言选择器切换到同一篇文章。
两种语言使用独立搜索索引，共享 `docs/assets/` 中的图片和下载文件。英文正文保留中文章节 ID，兼容既有跨文档锚点。

英文稿分批补齐时，未翻译的引用明确标为 `(Chinese)` 并链接中文正文；不会复制中文内容充当英文页。
英文首页显示实际覆盖率，空栏目说明待翻译。已核对的真实英文界面截图放在 `docs/assets/screenshots/en/`，仅英文正文引用；尚未复现的场景保留中文原图并说明图片语言。

翻译后记录已复核的中文 SHA-256 到 `translations.json`，然后运行：

```bash
python3 docs-site/check_translations.py
python3 docs-site/check_translations.py --require-complete
```

第一条检查已有译文的源版本、章节与代码块；第二条另要求所有中文文章都有英文稿。
`build.sh` 默认运行第二条：新增或修改中文文章后，必须补齐或复核对应英文稿，才能通过生产构建。
需要查看尚未完成的本地译稿时，可单独运行第一条和 `stage_docs.py`；部分译稿预览不代表全站验收通过。

保留命令语法、配置键、技术标识和真实输出；翻译说明性注释、交互提示、占位符与示例提问。
Mermaid 图的可见文字、纯文本公式、写作模板和目录说明也可翻译；
逐块复核结构、路径及技术标识后，将原块与英文块的 SHA-256 登记到 `localized-code-blocks.json`。
检查只接受已复核的精确内容组合，后续修改任一块都需重新复核，不能用刷新哈希代替内容审阅。

## Read the Docs 私有托管

仓库根目录的 `.readthedocs.yaml` 让 readthedocs.com 上的私有项目复用 `build.sh` 构建：下载 Hugo extended 0.165.0，用 `DOCS_BASE_URL` 把 baseURL 换成项目根地址（`hugo.yaml` 中的 `/docs/` 不变），输出到 `$READTHEDOCS_OUTPUT/html`。主题默认从公开的 GitHub 仓库下载，不需要设置 `WEBSITE_REPO`；本地验证同样的构建：

```bash
DOCS_BASE_URL=https://<项目>.readthedocs-hosted.com/ HUGO_BIN=/path/to/hugo bash docs-site/build.sh
```

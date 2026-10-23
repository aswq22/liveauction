# Live Auction 与 DeepSeek Harness

本仓库包含两个完整项目，保持它们的内部结构与相邻目录布局：

```text
liveauction/
├── ByteDanceliveauctioni/  # 竞拍后台、前端、H5、Agent Service 与 Harness 集成插件
└── deepseek-harness/      # 完整 DeepSeek Harness 源码、测试、文档和上游依赖源码
```

- [竞拍项目说明](ByteDanceliveauctioni/README.md)
- [竞拍服务启动说明](ByteDanceliveauctioni/STARTUP.md)
- [Harness 集成与启动](ByteDanceliveauctioni/integrations/deepseek-harness/README.md)
- [DeepSeek Harness 说明](deepseek-harness/README.zh.md)

两个项目都以普通源码目录收录，克隆本仓库即可获得全部实现，无需初始化 Git submodule。运行集成启动脚本时，先进入 `ByteDanceliveauctioni/`，脚本使用的相邻 `deepseek-harness/` 路径保持有效。

本地环境文件、API 密钥、私钥、依赖安装目录及构建缓存不收录。请按项目说明在本地设置环境变量。

许可证分别见 [竞拍项目 LICENSE](ByteDanceliveauctioni/LICENSE) 和 [DeepSeek Harness LICENSE](deepseek-harness/LICENSE)，第三方声明见 [THIRD_PARTY_NOTICES.md](deepseek-harness/THIRD_PARTY_NOTICES.md)。

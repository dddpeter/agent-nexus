# Security Policy / 安全策略

## 报告漏洞

请**不要**通过公开 Issue 报告安全漏洞。请通过 GitHub Security Advisories（"Report a vulnerability"）私密提交，或联系仓库所有者。

我们会在 72 小时内确认收到，7 天内给出评估结论。

## 安全设计要点（与设计文档对应）

- Agent 注册与驱动涉及本机进程执行：adapter 只执行配置文件中显式声明的命令，不接受消息内容拼接 shell
- 所有 agent 产出内容视为不可信输入，进入群聊前经过内容长度与格式校验
- 配置文件（含 API key 类字段）不入库，`.gitignore` 排除 `*.local.yaml` / `.env`
- 事件日志与消息存档可能包含敏感对话，存储目录权限默认 0600

## 范围

- 覆盖：nexus 主程序、adapter 实现、Web UI、部署脚本
- 不覆盖：被纳管的第三方 agent 自身的安全问题（请向各自上游报告）

export const portfolioSkill = Object.freeze({
  name: 'analyzing-auction-portfolio',
  description: 'Use when the user asks to汇总、比较或诊断多个独立竞拍商家账号；单账号查询或一次普通 Tool 即可完成时不要使用。',
  source: 'runtime',
  content: `# 多商家账号经营分析

## 使用边界

仅处理至少两个独立商家账号的汇总、比较或经营诊断。账号、密码或时间范围不完整时先追问，不猜测凭据。单账号问题使用普通查询 Tool。

## 工作方式

凭据完整后调用一次 \`auction_get_multi_account_report\`。由该 Tool 计算金额、平均值、比率、排名和风险；不要循环切换单账号登录，也不要自行聚合数字。

先读取 \`status\`、\`coverage\`、\`partialErrors\` 和 \`currency\`。结果为 \`PARTIAL\` 时，先说明 \`included/requested\` 覆盖率，再引用指标。结果为 \`ALL_ACCOUNTS_FAILED\` 时只说明失败范围，不生成经营结论。

回答按以下顺序组织：

1. 统计事实：总体指标、账号排名和成交拍品。
2. 风险信号：只把 Tool 返回的 \`riskFlags\` 称为系统识别的风险。
3. 分析推断：说明推断依据，不把排名靠后直接等同于异常。
4. 建议动作：给出与风险证据对应的可执行建议。

不要复述、保存或在后续调用中复制密码。需要单账号精确明细时，指出目标账号并重新使用正常的单账号登录与查询流程；组合 Tool 的临时登录态不可复用。

## 常见错误

- 百分比直接取平均：使用 Tool 返回的合并分子、分母和加权比率。
- 把各账号参拍人数相加称为唯一人数：组合结果不提供跨账号唯一参拍人数。
- 忽略部分失败：任何汇总结论都必须同时披露覆盖率。
`,
});

import { Clock3, LoaderCircle, Play, WalletCards, X } from 'lucide-react';
import { StudioBadge, StudioButton, StudioCard, StudioEmptyState } from '../../pages/host-console/components/studio-ui';
import type { AgentDraftCandidate } from './model/types';

type Props = {
  candidates: AgentDraftCandidate[];
  busy?: 'start' | 'close' | null;
  disabled?: boolean;
  onStart(): void;
  onClose(): void;
};

export function SessionPlanPanel({ candidates, busy = null, disabled = false, onStart, onClose }: Props) {
  const lots = [...candidates].sort((a, b) => a.position - b.position);
  const totalSeconds = lots.reduce((sum, item) => sum + item.candidate.draft.durationSeconds, 0);
  const prices = lots.map((item) => item.candidate.draft.startPriceAmount);
  const priceRange = prices.length ? `${yuan(Math.min(...prices))} – ${yuan(Math.max(...prices))}` : '—';

  return <StudioCard padding="lg" className="agentPlanPanel">
    <header className="agentPlanHeader">
      <div><span>Live plan</span><h2 id="agent-candidates-title">直播排品方案</h2><p>只展示开拍所需的价格与时间信息。</p></div>
      <StudioBadge tone="info">{lots.length} 件</StudioBadge>
    </header>
    {lots.length ? <>
      <div className="agentPlanSummary" aria-label="方案概览">
        <div><Clock3 size={18} /><span>预计总时长</span><strong>{minutes(totalSeconds)}</strong></div>
        <div><WalletCards size={18} /><span>起拍价范围</span><strong>{priceRange}</strong></div>
      </div>
      <ol className="agentPlanList" aria-label="拍品开拍顺序">
        {lots.map((item, index) => {
          const draft = item.candidate.draft;
          return <li key={item.candidate.id}>
            <span className="agentPlanOrder">{index + 1}</span>
            <div><strong>{draft.title}</strong><small>{index === 0 ? '开场' : index === lots.length - 1 ? '压轴' : '中场'}</small></div>
            <dl><div><dt>起拍价</dt><dd>{yuan(draft.startPriceAmount)}</dd></div>{draft.capPriceAmount ? <div><dt>封顶价</dt><dd>{yuan(draft.capPriceAmount)}</dd></div> : null}<div><dt>时长</dt><dd>{minutes(draft.durationSeconds)}</dd></div></dl>
          </li>;
        })}
      </ol>
      <div className="agentPlanActions">
        <p>执行后将按此顺序原子追加到队尾，不打断当前竞拍；所有拍品先进入等待开拍状态。</p>
        <div>
          <StudioButton type="button" variant="secondary" icon={busy === 'close' ? <LoaderCircle className="agentSpin" size={16} /> : <X size={16} />} disabled={disabled || Boolean(busy)} onClick={onClose}>取消方案</StudioButton>
          <StudioButton type="button" variant="primary" icon={busy === 'start' ? <LoaderCircle className="agentSpin" size={16} /> : <Play size={16} />} disabled={disabled || Boolean(busy)} onClick={onStart}>执行并加入队列</StudioButton>
        </div>
      </div>
    </> : <StudioEmptyState compact title="暂无可执行方案" description="请让 Agent 根据库存重新生成排品方案。" />}
  </StudioCard>;
}

function yuan(cents: number) {
  return `¥${(cents / 100).toLocaleString('zh-CN', { maximumFractionDigits: 2 })}`;
}

function minutes(seconds: number) {
  const value = seconds / 60;
  return `${Number.isInteger(value) ? value : value.toFixed(1)} 分钟`;
}

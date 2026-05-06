import React from 'react';
import './plugin-styles.css';

interface ProgressBarProps {
  total: number;
  mirrored: number;
  pending: number;
  failed: number;
}

export const ProgressBar: React.FC<ProgressBarProps> = ({ total, mirrored, pending, failed }) => {
  const t = Math.max(total, 1);
  const mPct = Math.min((mirrored / t) * 100, 100);
  const pPct = Math.min((pending / t) * 100, 100 - mPct);
  const fPct = Math.min((failed / t) * 100, 100 - mPct - pPct);
  const pct = Math.round((mirrored / t) * 100);

  return (
    <div>
      <div className="mirror-progress-stacked">
        <div className="mirror-progress-stacked__bar mirror-progress-stacked__bar--mirrored" style={{ width: `${mPct}%` }} />
        <div className="mirror-progress-stacked__bar mirror-progress-stacked__bar--pending" style={{ width: `${pPct}%` }} />
        <div className="mirror-progress-stacked__bar mirror-progress-stacked__bar--failed" style={{ width: `${fPct}%` }} />
      </div>
      <div className="mirror-progress-meta">
        <span>{mirrored.toLocaleString()} / {total.toLocaleString()}</span>
        <span>{pct}%</span>
      </div>
    </div>
  );
};

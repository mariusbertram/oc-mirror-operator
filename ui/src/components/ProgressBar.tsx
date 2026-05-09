import React from 'react';
import { Progress, ProgressSize } from '@patternfly/react-core';

interface ProgressBarProps {
  total: number;
  mirrored: number;
  pending: number;
  failed: number;
  /** When true, show only the bar without the count/percent meta row */
  compact?: boolean;
}

export const ProgressBar: React.FC<ProgressBarProps> = ({ total, mirrored, pending, failed, compact }) => {
  const t = Math.max(total, 1);
  const pct = Math.round((mirrored / t) * 100);

  return (
    <div>
      <Progress
        value={pct}
        size={ProgressSize.sm}
        aria-label="Mirror progress"
        style={{ minWidth: 120 }}
      />
      {!compact && (
        <div className="mirror-progress-meta">
          <span>{mirrored.toLocaleString()} / {total.toLocaleString()}</span>
          {pending > 0 && (
            <span style={{ color: 'var(--pf-v6-global--warning-color--100)' }}>
              {pending.toLocaleString()} pending
            </span>
          )}
          {failed > 0 && (
            <span style={{ color: 'var(--pf-v6-global--danger-color--100)' }}>
              {failed.toLocaleString()} failed
            </span>
          )}
        </div>
      )}
    </div>
  );
};

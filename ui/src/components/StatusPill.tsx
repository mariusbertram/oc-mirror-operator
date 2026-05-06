import React from 'react';
import './plugin-styles.css';

export type MirrorStatus = 'Healthy' | 'Mirroring' | 'Failed' | 'Unknown';

export function computeStatus(
  totalImages: number,
  mirroredImages: number,
  pendingImages: number,
  failedImages: number,
): MirrorStatus {
  if (failedImages > 0) return 'Failed';
  if (pendingImages > 0) return 'Mirroring';
  if (totalImages > 0 && mirroredImages >= totalImages) return 'Healthy';
  if (totalImages > 0) return 'Mirroring';
  return 'Unknown';
}

interface StatusPillProps {
  status: MirrorStatus;
}

export const StatusPill: React.FC<StatusPillProps> = ({ status }) => {
  const cls = {
    Healthy: 'mirror-status--healthy',
    Mirroring: 'mirror-status--mirroring',
    Failed: 'mirror-status--failed',
    Unknown: 'mirror-status--unknown',
  }[status];

  const label = {
    Healthy: 'Ready',
    Mirroring: 'Mirroring',
    Failed: 'Degraded',
    Unknown: 'Unknown',
  }[status];

  return (
    <span className={`mirror-status ${cls}`}>
      <span className="mirror-status-dot" />
      {label}
    </span>
  );
};

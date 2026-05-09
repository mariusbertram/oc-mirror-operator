import React from 'react';
import { Label } from '@patternfly/react-core';
import {
  CheckCircleIcon,
  ExclamationCircleIcon,
  InProgressIcon,
  QuestionCircleIcon,
} from '@patternfly/react-icons';

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

const STATUS_CONFIG: Record<MirrorStatus, { color: 'green' | 'blue' | 'red' | 'grey'; icon: React.ReactNode; label: string }> = {
  Healthy:   { color: 'green', icon: <CheckCircleIcon />,      label: 'Ready'     },
  Mirroring: { color: 'blue',  icon: <InProgressIcon />,       label: 'Mirroring' },
  Failed:    { color: 'red',   icon: <ExclamationCircleIcon />, label: 'Degraded'  },
  Unknown:   { color: 'grey',  icon: <QuestionCircleIcon />,   label: 'Unknown'   },
};

interface StatusPillProps {
  status: MirrorStatus;
}

export const StatusPill: React.FC<StatusPillProps> = ({ status }) => {
  const { color, icon, label } = STATUS_CONFIG[status];
  return (
    <Label color={color} icon={icon} isCompact>
      {label}
    </Label>
  );
};

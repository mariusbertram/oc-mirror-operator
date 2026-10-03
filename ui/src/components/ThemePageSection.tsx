import React from 'react';
import { PageSection as PatternFlyPageSection } from '@patternfly/react-core';

export const PageSection: React.FC<React.ComponentProps<typeof PatternFlyPageSection>> = ({ className, ...props }) => (
  <PatternFlyPageSection {...props} className={className ? `mirror-ui ${className}` : 'mirror-ui'} />
);

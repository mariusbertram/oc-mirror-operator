import React, { useEffect } from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl } from '../../api/client';
import { MirrorTargetDetail } from '../../pages/MirrorTargets/MirrorTargetDetail';

const MirrorTargetDetailPage: React.FC = () => {
  useEffect(() => {
    setFetchImpl((url, init) => consoleFetch(url, init));
  }, []);

  return <MirrorTargetDetail />;
};

export default MirrorTargetDetailPage;

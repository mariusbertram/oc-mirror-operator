import React, { useEffect } from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl } from '../../api/client';
import { MirrorTargetList } from '../../pages/MirrorTargets/MirrorTargetList';

// Wire consoleFetch as the HTTP transport so the plugin honours the
// OpenShift Console session token for all API calls.
const MirrorTargetListPage: React.FC = () => {
  useEffect(() => {
    setFetchImpl((url, init) => consoleFetch(url, init));
  }, []);

  return <MirrorTargetList />;
};

export default MirrorTargetListPage;

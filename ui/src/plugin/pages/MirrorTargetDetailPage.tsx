import React from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl, setApiBaseUrl } from '../../api/client';
import { MirrorTargetDetail } from '../../pages/MirrorTargets/MirrorTargetDetail';

setFetchImpl((url, init) => consoleFetch(url, init));
setApiBaseUrl('/api/proxy/plugin/oc-mirror-operator/resourceapi');

const MirrorTargetDetailPage: React.FC<any> = (props) => {
  console.log('MirrorTargetDetailPage wrapper props:', JSON.stringify(props));
  return <MirrorTargetDetail {...props} />;
};

export default MirrorTargetDetailPage;

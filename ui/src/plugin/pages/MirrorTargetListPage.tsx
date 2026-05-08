import React from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl, setApiBaseUrl } from '../../api/client';
import { MirrorTargetList } from '../../pages/MirrorTargets/MirrorTargetList';

// Configure the API client once at module load time, before any component renders.
// This avoids a race where child useEffects fire before the parent useEffect sets these up.
setFetchImpl((url, init) => consoleFetch(url, init));
setApiBaseUrl('/api/proxy/plugin/oc-mirror-operator/resourceapi');

const MirrorTargetListPage: React.FC<any> = (props) => <MirrorTargetList {...props} />;

export default MirrorTargetListPage;

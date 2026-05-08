import React, { useEffect, useState } from 'react';
import {
  Alert,
  Badge,
  Button,
  Card,
  CardBody,
  CardTitle,
  PageSection,
  Spinner,
  Tab,
  Tabs,
  TabTitleText,
  Title,
} from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { useParams } from 'react-router';
import { Link } from 'react-router-dom';
import { getTarget, triggerRecollect, deleteImageSet, getApiUrl } from '../../api/client';
import type { TargetDetail } from '../../api/types';
import { StatusPill, computeStatus } from '../../components/StatusPill';
import { ProgressBar } from '../../components/ProgressBar';
import '../../components/plugin-styles.css';

export const MirrorTargetDetail: React.FC = () => {
  const params = useParams<{ name: string }>();
  const name =
    params.name ||
    window.location.pathname.match(/\/oc-mirror\/targets\/([^/]+)/)?.[1];

  const [target, setTarget] = useState<TargetDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<string | number>('overview');

  const load = () => {
    if (!name) return;
    setLoading(true);
    getTarget(name)
      .then(setTarget)
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    load();
    const interval = setInterval(load, 30_000);
    return () => clearInterval(interval);
  }, [name]);

  const handleRecollect = async (namespace: string, isName: string) => {
    try {
      await triggerRecollect(namespace, isName);
    } catch (e) {
      alert(`Failed: ${(e as Error).message}`);
    }
  };

  const handleDelete = async (namespace: string, isName: string) => {
    if (!window.confirm(`Delete ImageSet ${isName}?`)) return;
    try {
      await deleteImageSet(namespace, isName);
      load();
    } catch (e) {
      alert(`Failed: ${(e as Error).message}`);
    }
  };

  if (loading && !target) {
    return <PageSection><Spinner /></PageSection>;
  }

  if (error) {
    return (
      <PageSection>
        <Alert variant="danger" title="Failed to load MirrorTarget" isInline>{error}</Alert>
      </PageSection>
    );
  }

  if (!target) return null;

  const overallStatus = computeStatus(target.totalImages, target.mirroredImages, target.pendingImages, target.failedImages);

  return (
    <>
      <PageSection style={{ paddingBottom: 0, borderBottom: '1px solid var(--pf-v6-global--BorderColor--100)' }}>
        <div style={{ marginBottom: 6 }}>
          <Link to="/oc-mirror/targets" style={{ fontSize: 13 }}>← MirrorTargets</Link>
          {' / '}
          <code style={{ fontSize: 13 }}>{name}</code>
        </div>
        <div className="mirror-row" style={{ marginBottom: 8 }}>
          <Title headingLevel="h1">{target.name}</Title>
          <code className="mirror-mono" style={{ marginLeft: 8, color: 'var(--pf-v6-global--Color--200)' }}>
            {target.registry}
          </code>
          <div className="mirror-spacer" />
          <StatusPill status={overallStatus} />
          <Button variant="secondary" size="sm" onClick={load} isDisabled={loading} style={{ marginLeft: 8 }}>
            Refresh
          </Button>
        </div>
      </PageSection>

      <PageSection padding={{ default: 'noPadding' }}>
        <Tabs
          activeKey={activeTab}
          onSelect={(_e, k) => setActiveTab(k)}
          style={{ paddingLeft: 24 }}
        >
          <Tab eventKey="overview" title={<TabTitleText>Overview</TabTitleText>} />
          <Tab eventKey="imagesets" title={<TabTitleText>ImageSets ({target.imageSets.length})</TabTitleText>} />
          <Tab eventKey="resources" title={<TabTitleText>Resources</TabTitleText>} />
          <Tab eventKey="catalogs" title={<TabTitleText>Catalogs</TabTitleText>} />
          <Tab eventKey="conditions" title={<TabTitleText>Conditions</TabTitleText>} />
        </Tabs>
      </PageSection>

      <PageSection>
        {activeTab === 'overview' && (
          <OverviewTab target={target} />
        )}
        {activeTab === 'imagesets' && (
          <ImageSetsTab
            target={target}
            onRecollect={handleRecollect}
            onDelete={handleDelete}
          />
        )}
        {activeTab === 'resources' && (
          <ResourcesTab target={target} />
        )}
        {activeTab === 'catalogs' && (
          <CatalogsTab target={target} />
        )}
        {activeTab === 'conditions' && (
          <ConditionsTab target={target} />
        )}
      </PageSection>
    </>
  );
};

const OverviewTab: React.FC<{ target: TargetDetail }> = ({ target }) => (
  <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
    <Card>
      <CardTitle>Mirror progress</CardTitle>
      <CardBody>
        <ProgressBar
          total={target.totalImages}
          mirrored={target.mirroredImages}
          pending={target.pendingImages}
          failed={target.failedImages}
        />
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 12, marginTop: 16 }}>
          <StatBox label="Total" value={target.totalImages} />
          <StatBox label="Mirrored" value={target.mirroredImages} color="var(--pf-v6-global--success-color--100)" />
          <StatBox label="Pending" value={target.pendingImages} color="var(--pf-v6-global--warning-color--100)" />
          <StatBox label="Failed" value={target.failedImages} color="var(--pf-v6-global--danger-color--100)" />
        </div>
      </CardBody>
    </Card>
    <Card>
      <CardTitle>Configuration</CardTitle>
      <CardBody>
        <dl className="mirror-kv">
          <dt>Namespace</dt><dd>{target.namespace}</dd>
          <dt>Registry</dt><dd><code className="mirror-mono">{target.registry}</code></dd>
          <dt>ImageSets</dt><dd>{target.imageSets.length} configured</dd>
        </dl>
      </CardBody>
    </Card>
    {target.resources.length > 0 && (
      <Card style={{ gridColumn: '1 / 3' }}>
        <CardTitle>Resource API</CardTitle>
        <CardBody>
          {target.resources.slice(0, 3).map((r) => (
            <div key={r.url} style={{ marginBottom: 4 }}>
              <a href={getApiUrl(r.url)} target="_blank" rel="noreferrer">{r.name}</a>
              {' '}
              <span className="mirror-tag">{r.type}</span>
            </div>
          ))}
          {target.resources.length > 3 && (
            <Link to="#" onClick={() => {}} style={{ fontSize: 13 }}>
              +{target.resources.length - 3} more — see Resources tab
            </Link>
          )}
        </CardBody>
      </Card>
    )}
  </div>
);

const StatBox: React.FC<{ label: string; value: number; color?: string }> = ({ label, value, color }) => (
  <div>
    <div style={{ fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.06em', color: 'var(--pf-v6-global--Color--200)', fontWeight: 600 }}>
      {label}
    </div>
    <div style={{ fontFamily: 'var(--pf-v6-global--FontFamily--heading--sans-serif)', fontSize: 26, fontWeight: 600, fontVariantNumeric: 'tabular-nums', color: color || 'inherit', letterSpacing: '-0.01em' }}>
      {value.toLocaleString()}
    </div>
  </div>
);

const ImageSetsTab: React.FC<{
  target: TargetDetail;
  onRecollect: (ns: string, name: string) => void;
  onDelete: (ns: string, name: string) => void;
}> = ({ target, onRecollect, onDelete }) => (
  <Card>
    <CardTitle>ImageSets ({target.imageSets.length})</CardTitle>
    <CardBody style={{ padding: 0 }}>
      {target.imageSets.length === 0 ? (
        <div style={{ padding: 24, color: 'var(--pf-v6-global--Color--200)' }}>No ImageSets configured.</div>
      ) : (
        <Table aria-label="ImageSets" variant="compact">
          <Thead>
            <Tr>
              <Th>Name</Th>
              <Th>Status</Th>
              <Th style={{ minWidth: 200 }}>Progress</Th>
              <Th>Total</Th>
              <Th>Mirrored</Th>
              <Th>Failed</Th>
              <Th>Actions</Th>
            </Tr>
          </Thead>
          <Tbody>
            {target.imageSets.map((is) => {
              const status = computeStatus(is.total, is.mirrored, is.pending, is.failed);
              return (
                <Tr key={is.name}>
                  <Td>
                    <Link to={`/oc-mirror/targets/${target.name}/imagesets/${is.name}`} style={{ fontWeight: 500 }}>
                      {is.name}
                    </Link>
                  </Td>
                  <Td><StatusPill status={status} /></Td>
                  <Td>
                    <ProgressBar total={is.total} mirrored={is.mirrored} pending={is.pending} failed={is.failed} />
                  </Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums' }}>{is.total.toLocaleString()}</Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums' }}>{is.mirrored.toLocaleString()}</Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums', color: is.failed > 0 ? 'var(--pf-v6-global--danger-color--100)' : undefined }}>
                    {is.failed.toLocaleString()}
                  </Td>
                  <Td>
                    <div className="mirror-row" style={{ gap: 6 }}>
                      <Button
                        variant="secondary"
                        size="sm"
                        onClick={() => onRecollect(target.namespace, is.name)}
                      >
                        Recollect
                      </Button>
                      {target.catalogs.length > 0 && (
                        <Link to={`/oc-mirror/targets/${target.name}/namespaces/${target.namespace}/imagesets/${is.name}/catalogs/${target.catalogs[0]?.slug}`}>
                          <Button variant="link" size="sm" style={{ paddingLeft: 0 }}>Browse catalog</Button>
                        </Link>
                      )}
                      <Button
                        variant="plain"
                        size="sm"
                        style={{ color: 'var(--pf-v6-global--danger-color--100)' }}
                        onClick={() => onDelete(target.namespace, is.name)}
                      >
                        Delete
                      </Button>
                    </div>
                  </Td>
                </Tr>
              );
            })}
          </Tbody>
        </Table>
      )}
    </CardBody>
  </Card>
);

const ResourcesTab: React.FC<{ target: TargetDetail }> = ({ target }) => {
  const allResources = [
    ...target.resources.map((r) => ({ ...r, imageSet: '' })),
    ...target.imageSets.flatMap((is) =>
      is.resources
        .filter((ir) => !target.resources.some((tr) => tr.url === ir.url))
        .map((r) => ({ ...r, imageSet: is.name })),
    ),
  ];

  if (allResources.length === 0) {
    return (
      <div style={{ color: 'var(--pf-v6-global--Color--200)', padding: 16 }}>
        No resources available yet. Resources are exposed once an ImageSet reaches Ready.
      </div>
    );
  }

  return (
    <Card>
      <CardTitle>Generated resources</CardTitle>
      <CardBody style={{ padding: 0 }}>
        <Table aria-label="Resources" variant="compact">
          <Thead>
            <Tr><Th>Resource</Th><Th>Type</Th><Th>URL</Th></Tr>
          </Thead>
          <Tbody>
            {allResources.map((r) => (
              <Tr key={`${r.imageSet}-${r.url}`}>
                <Td>
                  {r.imageSet && <span className="mirror-tag" style={{ marginRight: 6 }}>{r.imageSet}</span>}
                  {r.name}
                </Td>
                <Td><span className="mirror-tag">{r.type}</span></Td>
                <Td>
                  <a href={getApiUrl(r.url)} target="_blank" rel="noreferrer">
                    <code className="mirror-mono">{r.url}</code>
                  </a>
                </Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      </CardBody>
    </Card>
  );
};

const CatalogsTab: React.FC<{ target: TargetDetail }> = ({ target }) => {
  if (target.catalogs.length === 0) {
    return (
      <div style={{ color: 'var(--pf-v6-global--Color--200)', padding: 16 }}>
        No catalogs tracked by this MirrorTarget.
      </div>
    );
  }

  return (
    <Card>
      <CardTitle>Operator catalogs</CardTitle>
      <CardBody style={{ padding: 0 }}>
        <Table aria-label="Catalogs" variant="compact">
          <Thead>
            <Tr><Th>Slug</Th><Th>Source</Th><Th>Target image</Th><Th>Browse</Th></Tr>
          </Thead>
          <Tbody>
            {target.catalogs.map((c) => (
              <Tr key={c.slug}>
                <Td><strong>{c.slug}</strong></Td>
                <Td><code className="mirror-mono">{c.source}</code></Td>
                <Td><code className="mirror-mono">{c.targetImage}</code></Td>
                <Td>
                  {target.imageSets.length > 0 && (
                    <Link to={`/oc-mirror/targets/${target.name}/namespaces/${target.namespace}/imagesets/${target.imageSets[0]?.name}/catalogs/${c.slug}`}>
                      <Button variant="link" size="sm" style={{ paddingLeft: 0 }}>Browse packages</Button>
                    </Link>
                  )}
                </Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      </CardBody>
    </Card>
  );
};

const ConditionsTab: React.FC<{ target: TargetDetail }> = ({ target }) => {
  if (target.conditions.length === 0) {
    return <div style={{ color: 'var(--pf-v6-global--Color--200)', padding: 16 }}>No conditions reported.</div>;
  }

  return (
    <Card>
      <CardTitle>Conditions</CardTitle>
      <CardBody style={{ padding: 0 }}>
        <Table aria-label="Conditions" variant="compact">
          <Thead>
            <Tr><Th>Type</Th><Th>Status</Th><Th>Reason</Th><Th>Message</Th></Tr>
          </Thead>
          <Tbody>
            {target.conditions.map((c) => (
              <Tr key={c.type}>
                <Td><strong>{c.type}</strong></Td>
                <Td>
                  <Badge style={{
                    backgroundColor: c.status === 'True'
                      ? 'var(--pf-v6-global--success-color--100)'
                      : 'var(--pf-v6-global--danger-color--100)',
                    color: '#fff',
                  }}>
                    {c.status}
                  </Badge>
                </Td>
                <Td><code className="mirror-mono">{c.reason}</code></Td>
                <Td>{c.message}</Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      </CardBody>
    </Card>
  );
};

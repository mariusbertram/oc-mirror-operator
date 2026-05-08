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
import { getTarget, triggerRecollect } from '../../api/client';
import type { TargetDetail, ImageSetSummary } from '../../api/types';
import { StatusPill, computeStatus } from '../../components/StatusPill';
import { ProgressBar } from '../../components/ProgressBar';
import '../../components/plugin-styles.css';

type ImageSetDetailParams = 'targetName' | 'imageSetName';

export const ImageSetDetail: React.FC = () => {
  const params = useParams<ImageSetDetailParams>();
  let { targetName, imageSetName } = params;

  if (!targetName) {
    const m = window.location.pathname.match(/\/oc-mirror\/targets\/([^/]+)\/imagesets\/([^/]+)/);
    if (m) { targetName = m[1]; imageSetName = m[2]; }
  }

  const [target, setTarget] = useState<TargetDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<string | number>('overview');

  const load = () => {
    if (!targetName) return;
    setLoading(true);
    getTarget(targetName)
      .then(setTarget)
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    load();
    const interval = setInterval(load, 30_000);
    return () => clearInterval(interval);
  }, [targetName]);

  if (loading && !target) return <PageSection><Spinner /></PageSection>;
  if (error) return (
    <PageSection>
      <Alert variant="danger" title="Failed to load ImageSet" isInline>{error}</Alert>
    </PageSection>
  );

  const is: ImageSetSummary | undefined = target?.imageSets.find((s) => s.name === imageSetName);

  if (target && !is) {
    return (
      <PageSection>
        <Alert variant="warning" title={`ImageSet "${imageSetName}" not found in target "${targetName}"`} isInline />
      </PageSection>
    );
  }

  if (!is || !target) return null;

  const status = computeStatus(is.total, is.mirrored, is.pending, is.failed);
  const targetCatalogs = target.catalogs;

  return (
    <>
      <PageSection style={{ paddingBottom: 0, borderBottom: '1px solid var(--pf-v6-global--BorderColor--100)' }}>
        <div style={{ marginBottom: 6 }}>
          <Link to="/oc-mirror/imagesets" style={{ fontSize: 13 }}>← ImageSets</Link>
          {' / '}
          <Link to={`/oc-mirror/targets/${targetName}`} style={{ fontSize: 13 }}>{targetName}</Link>
          {' / '}
          <code style={{ fontSize: 13 }}>{imageSetName}</code>
        </div>
        <div className="mirror-row" style={{ marginBottom: 8 }}>
          <Title headingLevel="h1">{imageSetName}</Title>
          <div className="mirror-spacer" />
          <StatusPill status={status} />
          {targetCatalogs.length > 0 && (
            <Link to={`/oc-mirror/targets/${targetName}/namespaces/${target.namespace}/imagesets/${imageSetName}/catalogs/${targetCatalogs[0].slug}`}>
              <Button variant="secondary" size="sm">Browse catalog</Button>
            </Link>
          )}
          <Button
            variant="secondary"
            size="sm"
            onClick={() => triggerRecollect(target.namespace, imageSetName!).catch(console.error)}
          >
            Recollect
          </Button>
          <Button variant="secondary" size="sm" onClick={load} isDisabled={loading}>
            Refresh
          </Button>
        </div>
        <p style={{ margin: '0 0 12px', color: 'var(--pf-v6-global--Color--200)', fontSize: 13 }}>
          Targeting <span className="mirror-tag">{targetName}</span>
        </p>
      </PageSection>

      <PageSection padding={{ default: 'noPadding' }}>
        <Tabs
          activeKey={activeTab}
          onSelect={(_e, k) => setActiveTab(k)}
          style={{ borderBottom: '1px solid var(--pf-v6-global--BorderColor--100)', paddingLeft: 24 }}
        >
          <Tab eventKey="overview" title={<TabTitleText>Overview</TabTitleText>} />
          {is.resources.length > 0 && (
            <Tab eventKey="resources" title={<TabTitleText>Resources ({is.resources.length})</TabTitleText>} />
          )}
          {targetCatalogs.length > 0 && (
            <Tab eventKey="catalogs" title={<TabTitleText>Catalogs</TabTitleText>} />
          )}
        </Tabs>
      </PageSection>

      <PageSection>
        {activeTab === 'overview' && (
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
            <Card>
              <CardTitle>Mirroring progress</CardTitle>
              <CardBody>
                <ProgressBar total={is.total} mirrored={is.mirrored} pending={is.pending} failed={is.failed} />
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 12, marginTop: 16 }}>
                  <StatBox label="Total" value={is.total} />
                  <StatBox label="Mirrored" value={is.mirrored} color="var(--pf-v6-global--success-color--100)" />
                  <StatBox label="Pending" value={is.pending} color="var(--pf-v6-global--warning-color--100)" />
                  <StatBox label="Failed" value={is.failed} color="var(--pf-v6-global--danger-color--100)" />
                </div>
              </CardBody>
            </Card>
            <Card>
              <CardTitle>Details</CardTitle>
              <CardBody>
                <dl className="mirror-kv">
                  <dt>Name</dt><dd>{is.name}</dd>
                  <dt>MirrorTarget</dt>
                  <dd>
                    <Link to={`/oc-mirror/targets/${targetName}`}>
                      <span className="mirror-tag">{targetName}</span>
                    </Link>
                  </dd>
                  <dt>Namespace</dt><dd>{target.namespace}</dd>
                  <dt>Found</dt>
                  <dd>
                    <Badge style={{ background: is.found ? 'var(--pf-v6-global--success-color--100)' : 'var(--pf-v6-global--danger-color--100)', color: '#fff' }}>
                      {is.found ? 'Yes' : 'No'}
                    </Badge>
                  </dd>
                  <dt>Resources</dt><dd>{is.resources.length} available</dd>
                </dl>
              </CardBody>
            </Card>
          </div>
        )}

        {activeTab === 'resources' && (
          <Card>
            <CardTitle>Generated resources</CardTitle>
            <CardBody style={{ padding: 0 }}>
              <Table aria-label="Resources" variant="compact">
                <Thead>
                  <Tr><Th>Resource</Th><Th>Type</Th><Th>URL</Th></Tr>
                </Thead>
                <Tbody>
                  {is.resources.map((r) => (
                    <Tr key={r.url}>
                      <Td>{r.name}</Td>
                      <Td><span className="mirror-tag">{r.type}</span></Td>
                      <Td>
                        <a href={r.url} target="_blank" rel="noreferrer">
                          <code className="mirror-mono">{r.url}</code>
                        </a>
                      </Td>
                    </Tr>
                  ))}
                </Tbody>
              </Table>
            </CardBody>
          </Card>
        )}

        {activeTab === 'catalogs' && (
          <Card>
            <CardTitle>Operator catalogs</CardTitle>
            <CardBody style={{ padding: 0 }}>
              <Table aria-label="Catalogs" variant="compact">
                <Thead>
                  <Tr><Th>Slug</Th><Th>Source</Th><Th>Browse</Th></Tr>
                </Thead>
                <Tbody>
                  {targetCatalogs.map((c) => (
                    <Tr key={c.slug}>
                      <Td><strong>{c.slug}</strong></Td>
                      <Td><code className="mirror-mono">{c.source}</code></Td>
                      <Td>
                        <Link to={`/oc-mirror/targets/${targetName}/namespaces/${target.namespace}/imagesets/${imageSetName}/catalogs/${c.slug}`}>
                          <Button variant="link" size="sm" style={{ paddingLeft: 0 }}>Browse packages</Button>
                        </Link>
                      </Td>
                    </Tr>
                  ))}
                </Tbody>
              </Table>
            </CardBody>
          </Card>
        )}
      </PageSection>
    </>
  );
};

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

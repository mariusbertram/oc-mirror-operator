import React, { useEffect, useState } from 'react';
import {
  Alert,
  Breadcrumb,
  BreadcrumbItem,
  Button,
  Card,
  CardBody,
  CardTitle,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  Label,
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
import { ResourcesView } from '../../components/ResourcesView';
import '../../components/plugin-styles.css';

type ImageSetDetailParams = 'targetName' | 'imageSetName';

const StatBox: React.FC<{ label: string; value: number; color?: string }> = ({ label, value, color }) => (
  <div>
    <div className="mirror-stat-label">{label}</div>
    <div className="mirror-stat-value" style={color ? { color } : undefined}>
      {value.toLocaleString()}
    </div>
  </div>
);

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
      <PageSection>
        <Breadcrumb style={{ marginBottom: 'var(--pf-v6-global--spacer--md)' }}>
          <BreadcrumbItem><Link to="/oc-mirror/imagesets">ImageSets</Link></BreadcrumbItem>
          <BreadcrumbItem><Link to={`/oc-mirror/targets/${targetName}`}>{targetName}</Link></BreadcrumbItem>
          <BreadcrumbItem isActive>{imageSetName}</BreadcrumbItem>
        </Breadcrumb>

        <Flex alignItems={{ default: 'alignItemsCenter' }} style={{ rowGap: 'var(--pf-v6-global--spacer--sm)' }}>
          <FlexItem>
            <Title headingLevel="h1">{imageSetName}</Title>
          </FlexItem>
          <FlexItem align={{ default: 'alignRight' }}>
            <Flex gap={{ default: 'gapSm' }} alignItems={{ default: 'alignItemsCenter' }}>
              <FlexItem><StatusPill status={status} /></FlexItem>
              {targetCatalogs.length > 0 && (
                <FlexItem>
                  <Link to={`/oc-mirror/targets/${targetName}/namespaces/${target.namespace}/imagesets/${imageSetName}/catalogs/${targetCatalogs[0].slug}`}>
                    <Button variant="secondary" size="sm">Browse catalog</Button>
                  </Link>
                </FlexItem>
              )}
              <FlexItem>
                <Link to={`/oc-mirror/targets/${targetName}/namespaces/${target.namespace}/imagesets/${imageSetName}/releases`}>
                  <Button variant="secondary" size="sm">Browse releases</Button>
                </Link>
              </FlexItem>
              <FlexItem>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => triggerRecollect(target.namespace, imageSetName!).catch(console.error)}
                >
                  Recollect
                </Button>
              </FlexItem>
              <FlexItem>
                <Button variant="secondary" size="sm" onClick={load} isDisabled={loading}>
                  Refresh
                </Button>
              </FlexItem>
            </Flex>
          </FlexItem>
        </Flex>
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
          <div className="mirror-overview-grid">
            <Card>
              <CardTitle>Mirroring progress</CardTitle>
              <CardBody>
                <ProgressBar total={is.total} mirrored={is.mirrored} pending={is.pending} failed={is.failed} />
                <div className="mirror-stat-grid">
                  <StatBox label="Total"    value={is.total} />
                  <StatBox label="Mirrored" value={is.mirrored} color="var(--pf-v6-global--success-color--100)" />
                  <StatBox label="Pending"  value={is.pending}  color="var(--pf-v6-global--warning-color--100)" />
                  <StatBox label="Failed"   value={is.failed}   color="var(--pf-v6-global--danger-color--100)"  />
                </div>
              </CardBody>
            </Card>

            <Card>
              <CardTitle>Details</CardTitle>
              <CardBody>
                <DescriptionList isCompact>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Name</DescriptionListTerm>
                    <DescriptionListDescription>{is.name}</DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>MirrorTarget</DescriptionListTerm>
                    <DescriptionListDescription>
                      <Link to={`/oc-mirror/targets/${targetName}`}>
                        <Label isCompact color="grey">{targetName}</Label>
                      </Link>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Namespace</DescriptionListTerm>
                    <DescriptionListDescription>{target.namespace}</DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Found</DescriptionListTerm>
                    <DescriptionListDescription>
                      <Label isCompact color={is.found ? 'green' : 'red'}>
                        {is.found ? 'Yes' : 'No'}
                      </Label>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Resources</DescriptionListTerm>
                    <DescriptionListDescription>{is.resources.length} available</DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </CardBody>
            </Card>
          </div>
        )}

        {activeTab === 'resources' && (
          <ResourcesView resources={is.resources} />
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
                      <Td dataLabel="Slug"><strong>{c.slug}</strong></Td>
                      <Td dataLabel="Source"><code className="mirror-mono">{c.source}</code></Td>
                      <Td dataLabel="Browse">
                        <Link to={`/oc-mirror/targets/${targetName}/namespaces/${target.namespace}/imagesets/${imageSetName}/catalogs/${c.slug}`}>
                          <Button variant="link" size="sm" isInline>Browse packages</Button>
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

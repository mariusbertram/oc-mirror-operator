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
  TextInput,
  Title,
} from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { useParams } from 'react-router';
import { Link } from 'react-router-dom';
import { getBlockedImages, getTarget, patchBlockedImages, triggerRecollect } from '../../api/client';
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

  const [blockedImages, setBlockedImages] = useState<string[]>([]);
  const [blockedLoading, setBlockedLoading] = useState(true);
  const [blockedSaving, setBlockedSaving] = useState(false);
  const [blockedDirty, setBlockedDirty] = useState(false);
  const [blockedError, setBlockedError] = useState<string | null>(null);
  const [newBlockedName, setNewBlockedName] = useState('');

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

  useEffect(() => {
    if (!target?.namespace || !imageSetName) return;
    setBlockedLoading(true);
    getBlockedImages(target.namespace, imageSetName)
      .then((spec) => {
        setBlockedImages(spec.blockedImages ?? []);
        setBlockedDirty(false);
      })
      .catch((e: Error) => setBlockedError(e.message))
      .finally(() => setBlockedLoading(false));
  }, [target?.namespace, imageSetName]);

  const addBlockedImage = () => {
    const name = newBlockedName.trim();
    if (!name || blockedImages.includes(name)) return;
    setBlockedImages((prev) => [...prev, name]);
    setNewBlockedName('');
    setBlockedDirty(true);
  };

  const removeBlockedImage = (name: string) => {
    setBlockedImages((prev) => prev.filter((n) => n !== name));
    setBlockedDirty(true);
  };

  const saveBlockedImages = async () => {
    if (!target?.namespace || !imageSetName) return;
    setBlockedSaving(true);
    setBlockedError(null);
    try {
      await patchBlockedImages(target.namespace, imageSetName, blockedImages);
      setBlockedDirty(false);
    } catch (e: unknown) {
      setBlockedError(e instanceof Error ? e.message : String(e));
    } finally {
      setBlockedSaving(false);
    }
  };

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
  const imageSetCatalogs = targetCatalogs.filter((c) => is.catalogs.includes(c.slug));

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
          {imageSetCatalogs.length > 0 && (
            <Tab eventKey="catalogs" title={<TabTitleText>Catalogs</TabTitleText>} />
          )}
          <Tab eventKey="blocked" title={<TabTitleText>Blocked Images{blockedImages.length > 0 ? ` (${blockedImages.length})` : ''}</TabTitleText>} />
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
                  {imageSetCatalogs.map((c) => (
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

        {activeTab === 'blocked' && (
          <Card>
            <CardTitle>Blocked images</CardTitle>
            <CardBody>
              <Content component="p">
                Images matching a blocked name are excluded from mirroring across all
                content types (releases, operator catalogs, additional images).
                Matching is done on the repository path, ignoring registry host and tag
                (e.g. <code className="mirror-mono">redhat/postgresql-operator-bundle</code>).
              </Content>
              {blockedError && (
                <Alert variant="danger" title="Failed to load or save blocked images" isInline style={{ marginBottom: 16 }}>
                  {blockedError}
                </Alert>
              )}
              {blockedLoading ? (
                <Spinner size="md" />
              ) : (
                <>
                  <Flex style={{ marginBottom: 16 }} alignItems={{ default: 'alignItemsFlexEnd' }}>
                    <FlexItem grow={{ default: 'grow' }}>
                      <TextInput
                        aria-label="Image name to block"
                        placeholder="e.g. quay.io/org/repo"
                        value={newBlockedName}
                        onChange={(_e, v) => setNewBlockedName(v)}
                        onKeyDown={(e) => { if (e.key === 'Enter') addBlockedImage(); }}
                      />
                    </FlexItem>
                    <FlexItem>
                      <Button variant="secondary" onClick={addBlockedImage} isDisabled={!newBlockedName.trim()}>
                        Add
                      </Button>
                    </FlexItem>
                  </Flex>

                  <Table aria-label="Blocked images" variant="compact">
                    <Thead>
                      <Tr><Th>Name</Th><Th screenReaderText="Actions" /></Tr>
                    </Thead>
                    <Tbody>
                      {blockedImages.map((name) => (
                        <Tr key={name}>
                          <Td dataLabel="Name"><code className="mirror-mono">{name}</code></Td>
                          <Td dataLabel="Actions">
                            <Button variant="plain" size="sm" onClick={() => removeBlockedImage(name)} aria-label={`Remove ${name}`}>
                              ×
                            </Button>
                          </Td>
                        </Tr>
                      ))}
                      {blockedImages.length === 0 && (
                        <Tr><Td colSpan={2}>No blocked images configured.</Td></Tr>
                      )}
                    </Tbody>
                  </Table>

                  <Button
                    variant="primary"
                    style={{ marginTop: 16 }}
                    onClick={saveBlockedImages}
                    isDisabled={!blockedDirty || blockedSaving}
                    isLoading={blockedSaving}
                  >
                    Save
                  </Button>
                </>
              )}
            </CardBody>
          </Card>
        )}
      </PageSection>
    </>
  );
};

import React, { useEffect, useState } from 'react';
import {
  Alert,
  Breadcrumb,
  BreadcrumbItem,
  Button,
  Card,
  CardBody,
  CardTitle,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  Label,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Spinner,
  Tab,
  Tabs,
  TabTitleText,
  Text,
  TextContent,
  TextVariants,
} from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { useParams } from 'react-router';
import { Link } from 'react-router-dom';
import { getTarget, triggerRecollect, deleteImageSet } from '../../api/client';
import type { TargetDetail } from '../../api/types';
import { StatusPill, computeStatus } from '../../components/StatusPill';
import { ProgressBar } from '../../components/ProgressBar';
import { ResourcesView } from '../../components/ResourcesView';
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
  const [deleteCandidate, setDeleteCandidate] = useState<{ ns: string; name: string } | null>(null);

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

  const confirmDelete = async () => {
    if (!deleteCandidate) return;
    try {
      await deleteImageSet(deleteCandidate.ns, deleteCandidate.name);
      setDeleteCandidate(null);
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
      <PageSection>
        <Breadcrumb style={{ marginBottom: 'var(--pf-v6-global--spacer--md)' }}>
          <BreadcrumbItem><Link to="/oc-mirror/targets">MirrorTargets</Link></BreadcrumbItem>
          <BreadcrumbItem isActive>{name}</BreadcrumbItem>
        </Breadcrumb>

        <Flex alignItems={{ default: 'alignItemsCenter' }} style={{ rowGap: 'var(--pf-v6-global--spacer--sm)' }}>
          <FlexItem>
            <TextContent>
              <Text component={TextVariants.h1}>{target.name}</Text>
            </TextContent>
          </FlexItem>
          <FlexItem>
            <code className="mirror-mono mirror-registry-label">{target.registry}</code>
          </FlexItem>
          <FlexItem align={{ default: 'alignRight' }}>
            <Flex gap={{ default: 'gapSm' }} alignItems={{ default: 'alignItemsCenter' }}>
              <FlexItem><StatusPill status={overallStatus} /></FlexItem>
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
            onDelete={(ns, n) => setDeleteCandidate({ ns, name: n })}
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

      <Modal
        variant={ModalVariant.small}
        isOpen={deleteCandidate !== null}
        onClose={() => setDeleteCandidate(null)}
        aria-label="Delete ImageSet"
      >
        <ModalHeader title="Delete ImageSet?" titleIconVariant="warning" />
        <ModalBody>
          <p>
            Are you sure you want to delete <strong>{deleteCandidate?.name}</strong>?
            This will remove the ImageSet and all associated mirroring state.
          </p>
        </ModalBody>
        <ModalFooter>
          <Button variant="danger" onClick={confirmDelete}>Delete</Button>
          <Button variant="link" onClick={() => setDeleteCandidate(null)}>Cancel</Button>
        </ModalFooter>
      </Modal>
    </>
  );
};

const StatBox: React.FC<{ label: string; value: number; color?: string }> = ({ label, value, color }) => (
  <div>
    <div className="mirror-stat-label">{label}</div>
    <div className="mirror-stat-value" style={color ? { color } : undefined}>
      {value.toLocaleString()}
    </div>
  </div>
);

const OverviewTab: React.FC<{ target: TargetDetail }> = ({ target }) => (
  <div className="mirror-overview-grid">
    <Card>
      <CardTitle>Mirror progress</CardTitle>
      <CardBody>
        <ProgressBar
          total={target.totalImages}
          mirrored={target.mirroredImages}
          pending={target.pendingImages}
          failed={target.failedImages}
        />
        <div className="mirror-stat-grid">
          <StatBox label="Total"    value={target.totalImages} />
          <StatBox label="Mirrored" value={target.mirroredImages} color="var(--pf-v6-global--success-color--100)" />
          <StatBox label="Pending"  value={target.pendingImages}  color="var(--pf-v6-global--warning-color--100)" />
          <StatBox label="Failed"   value={target.failedImages}   color="var(--pf-v6-global--danger-color--100)"  />
        </div>
      </CardBody>
    </Card>

    <Card>
      <CardTitle>Configuration</CardTitle>
      <CardBody>
        <DescriptionList isCompact>
          <DescriptionListGroup>
            <DescriptionListTerm>Namespace</DescriptionListTerm>
            <DescriptionListDescription>{target.namespace}</DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Registry</DescriptionListTerm>
            <DescriptionListDescription>
              <code className="mirror-mono">{target.registry}</code>
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>ImageSets</DescriptionListTerm>
            <DescriptionListDescription>{target.imageSets.length} configured</DescriptionListDescription>
          </DescriptionListGroup>
        </DescriptionList>
      </CardBody>
    </Card>

    {target.resources.length > 0 && (
      <Card className="mirror-overview-full">
        <CardTitle>Resources</CardTitle>
        <CardBody>
          <ResourcesView resources={target.resources} />
        </CardBody>
      </Card>
    )}
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
        <div className="mirror-empty-body">No ImageSets configured.</div>
      ) : (
        <Table aria-label="ImageSets" variant="compact">
          <Thead>
            <Tr>
              <Th>Name</Th>
              <Th>Status</Th>
              <Th style={{ minWidth: 220 }}>Progress</Th>
              <Th>Actions</Th>
            </Tr>
          </Thead>
          <Tbody>
            {target.imageSets.map((is) => {
              const status = computeStatus(is.total, is.mirrored, is.pending, is.failed);
              return (
                <Tr key={is.name}>
                  <Td dataLabel="Name">
                    <Link to={`/oc-mirror/targets/${target.name}/imagesets/${is.name}`} className="mirror-link-strong">
                      {is.name}
                    </Link>
                  </Td>
                  <Td dataLabel="Status">
                    <StatusPill status={status} />
                  </Td>
                  <Td dataLabel="Progress">
                    <ProgressBar total={is.total} mirrored={is.mirrored} pending={is.pending} failed={is.failed} />
                  </Td>
                  <Td dataLabel="Actions">
                    <Flex gap={{ default: 'gapSm' }} flexWrap={{ default: 'nowrap' }}>
                      <FlexItem>
                        <Button variant="secondary" size="sm" onClick={() => onRecollect(target.namespace, is.name)}>
                          Recollect
                        </Button>
                      </FlexItem>
                      {target.catalogs.length > 0 && (
                        <FlexItem>
                          <Link to={`/oc-mirror/targets/${target.name}/namespaces/${target.namespace}/imagesets/${is.name}/catalogs/${target.catalogs[0]?.slug}`}>
                            <Button variant="link" size="sm" isInline>Browse catalog</Button>
                          </Link>
                        </FlexItem>
                      )}
                      <FlexItem>
                        <Button
                          variant="link"
                          size="sm"
                          isDanger
                          isInline
                          onClick={() => onDelete(target.namespace, is.name)}
                        >
                          Delete
                        </Button>
                      </FlexItem>
                    </Flex>
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
    ...target.resources,
    ...target.imageSets.flatMap((is) =>
      is.resources.filter((ir) => !target.resources.some((tr) => tr.url === ir.url)),
    ),
  ];
  return <ResourcesView resources={allResources} />;
};

const CatalogsTab: React.FC<{ target: TargetDetail }> = ({ target }) => {
  if (target.catalogs.length === 0) {
    return <div className="mirror-empty-body">No catalogs tracked by this MirrorTarget.</div>;
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
                <Td dataLabel="Slug"><strong>{c.slug}</strong></Td>
                <Td dataLabel="Source"><code className="mirror-mono">{c.source}</code></Td>
                <Td dataLabel="Target image"><code className="mirror-mono">{c.targetImage}</code></Td>
                <Td dataLabel="Browse">
                  {target.imageSets.length > 0 && (
                    <Link to={`/oc-mirror/targets/${target.name}/namespaces/${target.namespace}/imagesets/${target.imageSets[0]?.name}/catalogs/${c.slug}`}>
                      <Button variant="link" size="sm" isInline>Browse packages</Button>
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
    return <div className="mirror-empty-body">No conditions reported.</div>;
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
                <Td dataLabel="Type"><strong>{c.type}</strong></Td>
                <Td dataLabel="Status">
                  <Label
                    isCompact
                    color={c.status === 'True' ? 'green' : 'red'}
                  >
                    {c.status}
                  </Label>
                </Td>
                <Td dataLabel="Reason"><code className="mirror-mono">{c.reason}</code></Td>
                <Td dataLabel="Message">{c.message}</Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      </CardBody>
    </Card>
  );
};

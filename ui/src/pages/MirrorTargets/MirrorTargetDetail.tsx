import React, { useEffect, useState } from 'react';
import {
  Alert,
  Badge,
  Button,
  Card,
  CardBody,
  CardTitle,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Label,
  PageSection,
  Spinner,
  Title,
} from '@patternfly/react-core';
import { Link, useParams, RouteComponentProps } from 'react-router-dom';
import { getTarget, triggerRecollect, deleteImageSet } from '../../api/client';
import type { TargetDetail } from '../../api/types';

export const MirrorTargetDetail: React.FC<Partial<RouteComponentProps<{ name: string }>>> = ({ match }) => {
  const params = useParams<{ name: string }>();
  const name = match?.params?.name || params.name;
  console.log('MirrorTargetDetail rendering, name:', name);
  const [target, setTarget] = useState<TargetDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

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
      alert(`Recollect triggered for ${isName}`);
    } catch (e) {
      alert(`Failed: ${(e as Error).message}`);
    }
  };

  const handleDelete = async (namespace: string, isName: string) => {
    if (!window.confirm(`Are you sure you want to delete ImageSet ${isName}?`)) return;
    try {
      await deleteImageSet(namespace, isName);
      alert(`ImageSet ${isName} deleted`);
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

  return (
    <PageSection>
      <Link to="/oc-mirror/targets">← Back to MirrorTargets</Link>
      <Title headingLevel="h1" style={{ margin: '1rem 0' }}>{target.name}</Title>

      <Card style={{ marginBottom: '1rem' }}>
        <CardTitle>Overview</CardTitle>
        <CardBody>
          <DescriptionList isHorizontal>
            <DescriptionListGroup>
              <DescriptionListTerm>Namespace</DescriptionListTerm>
              <DescriptionListDescription>{target.namespace}</DescriptionListDescription>
            </DescriptionListGroup>
            <DescriptionListGroup>
              <DescriptionListTerm>Registry</DescriptionListTerm>
              <DescriptionListDescription>{target.registry}</DescriptionListDescription>
            </DescriptionListGroup>
            <DescriptionListGroup>
              <DescriptionListTerm>Total / Mirrored / Pending / Failed</DescriptionListTerm>
              <DescriptionListDescription>
                {target.totalImages} / {target.mirroredImages} / {target.pendingImages} /{' '}
                <span style={{ color: target.failedImages > 0 ? 'var(--pf-v5-global--danger-color--100)' : undefined }}>
                  {target.failedImages}
                </span>
                {(target.failedImages > 0 || target.pendingImages > 0) && (
                  <Link to={`/oc-mirror/targets/${target.name}/failures`} style={{ marginLeft: '1rem' }}>
                    <Button variant="link" isInline size="sm">View Details</Button>
                  </Link>
                )}
              </DescriptionListDescription>
            </DescriptionListGroup>
          </DescriptionList>
        </CardBody>
      </Card>

      <Card style={{ marginBottom: '1rem' }}>
        <CardTitle>Conditions</CardTitle>
        <CardBody>
          {target.conditions.length === 0 ? (
            <span>No conditions</span>
          ) : (
            target.conditions.map((c) => (
              <Label
                key={c.type}
                color={c.status === 'True' ? 'green' : 'red'}
                style={{ marginRight: '0.5rem', marginBottom: '0.5rem' }}
              >
                {c.type}: {c.reason}
              </Label>
            ))
          )}
        </CardBody>
      </Card>

      <Card style={{ marginBottom: '1rem' }}>
        <CardTitle>ImageSets</CardTitle>
        <CardBody>
          {target.imageSets.map((is) => (
            <Card key={is.name} isFlat style={{ marginBottom: '0.5rem' }}>
              <CardBody>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                  <strong>{is.name}</strong>
                  <div>
                    <Badge style={{ marginRight: '0.5rem' }}>{is.total} total</Badge>
                    <Badge isRead style={{ marginRight: '0.5rem', backgroundColor: '#1e8a00' }}>
                      {is.mirrored} mirrored
                    </Badge>
                    {is.failed > 0 && (
                      <Badge isRead style={{ backgroundColor: 'var(--pf-v5-global--danger-color--100)' }}>
                        {is.failed} failed
                      </Badge>
                    )}
                    <Button
                      variant="secondary"
                      size="sm"
                      style={{ marginLeft: '1rem' }}
                      onClick={() => handleRecollect(target.namespace, is.name)}
                    >
                      Recollect
                    </Button>
                    <Button
                      variant="danger"
                      size="sm"
                      style={{ marginLeft: '0.5rem' }}
                      onClick={() => handleDelete(target.namespace, is.name)}
                    >
                      Delete
                    </Button>
                  </div>
                </div>
              </CardBody>
            </Card>
          ))}
        </CardBody>
      </Card>

      {target.catalogs.length > 0 && (
        <Card style={{ marginBottom: '1rem' }}>
          <CardTitle>Catalogs</CardTitle>
          <CardBody>
            <Alert variant="info" title="Catalog Browser" isInline style={{ marginBottom: '1rem' }}>
              Select an ImageSet to edit catalog package filters.
            </Alert>
            {target.imageSets.map((is) => (
              <div key={is.name} style={{ marginBottom: '1rem' }}>
                <Title headingLevel="h4">{is.name}</Title>
                <div style={{ display: 'flex', gap: '1rem', flexWrap: 'wrap', marginTop: '0.5rem' }}>
                  {target.catalogs.map((c) => (
                    <Link
                      key={c.slug}
                      to={`/oc-mirror/targets/${target.name}/namespaces/${target.namespace}/imagesets/${is.name}/catalogs/${c.slug}`}
                    >
                      <Button variant="link" size="sm" style={{ paddingLeft: 0 }}>
                        Browse {c.slug}
                      </Button>
                    </Link>
                  ))}
                </div>
              </div>
            ))}
          </CardBody>
        </Card>
      )}

      {target.resources.length > 0 && (
        <Card>
          <CardTitle>Resources</CardTitle>
          <CardBody>
            {target.resources.map((r) => (
              <div key={r.url} style={{ marginBottom: '0.25rem' }}>
                <a href={r.url} target="_blank" rel="noreferrer">{r.name}</a>
                {' '}
                <Badge isRead>{r.type}</Badge>
              </div>
            ))}
          </CardBody>
        </Card>
      )}
    </PageSection>
  );
};

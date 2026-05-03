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
import { Link, useParams } from 'react-router-dom';
import { getTarget, triggerRecollect } from '../../api/client';
import type { TargetDetail } from '../../api/types';

export const MirrorTargetDetail: React.FC = () => {
  const { name } = useParams<{ name: string }>();
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
      <Link to="/targets">← Back to MirrorTargets</Link>
      <Title headingLevel="h1" style={{ margin: '1rem 0' }}>{target.name}</Title>

      <Card style={{ marginBottom: '1rem' }}>
        <CardTitle>Overview</CardTitle>
        <CardBody>
          <DescriptionList isHorizontal>
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
                      onClick={() => handleRecollect(/* namespace from URL */ 'default', is.name)}
                    >
                      Recollect
                    </Button>
                    <Link to={`/imagesets/${is.name}`} style={{ marginLeft: '0.5rem' }}>
                      <Button variant="link" size="sm">Edit</Button>
                    </Link>
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
            {target.catalogs.map((c) => (
              <div key={c.slug} style={{ marginBottom: '0.5rem' }}>
                <Link to={`/targets/${target.name}/catalogs/${c.slug}`}>
                  {c.slug}
                </Link>
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

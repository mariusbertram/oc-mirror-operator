import React, { useEffect, useState } from 'react';
import {
  Alert,
  Button,
  Checkbox,
  PageSection,
  SearchInput,
  Spinner,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core';
import { useParams } from 'react-router-dom';
import {
  getFilteredPackages,
  getUpstreamPackages,
  patchCatalogPackages,
} from '../../api/client';
import type { CatalogPackage } from '../../api/types';

export const CatalogBrowser: React.FC = () => {
  const { targetName, slug, namespace, imageSetName } = useParams<{
    targetName: string;
    slug: string;
    namespace: string;
    imageSetName: string;
  }>();

  const [upstream, setUpstream] = useState<CatalogPackage[]>([]);
  const [filtered, setFiltered] = useState<Set<string>>(new Set());
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [search, setSearch] = useState('');
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [successMsg, setSuccessMsg] = useState<string | null>(null);

  useEffect(() => {
    if (!targetName || !slug) return;
    setLoading(true);
    Promise.all([
      getUpstreamPackages(targetName, slug),
      getFilteredPackages(targetName, slug),
    ])
      .then(([up, fp]) => {
        setUpstream(up);
        const filteredNames = new Set(fp.map((p) => p.name));
        setFiltered(filteredNames);
        setSelected(new Set(filteredNames));
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  }, [targetName, slug]);

  const togglePackage = (name: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(name)) {
        next.delete(name);
      } else {
        next.add(name);
      }
      return next;
    });
  };

  const handleSave = async () => {
    if (!namespace || !imageSetName || !slug) return;
    setSaving(true);
    setError(null);
    setSuccessMsg(null);

    const allNames = upstream.map((p) => p.name);
    const include = allNames.filter((n) => selected.has(n));
    const exclude = allNames.filter((n) => !selected.has(n));

    try {
      await patchCatalogPackages(namespace, imageSetName, slug, { include, exclude });
      setFiltered(new Set(include));
      setSuccessMsg(`Saved. ${include.length} packages included, ${exclude.length} excluded.`);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const visiblePackages = upstream.filter(
    (p) => !search || p.name.toLowerCase().includes(search.toLowerCase()),
  );

  const hasChanges =
    selected.size !== filtered.size ||
    [...selected].some((n) => !filtered.has(n));

  if (loading) return <PageSection><Spinner /></PageSection>;

  return (
    <PageSection>
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Catalog: {slug}
      </Title>

      {error && (
        <Alert variant="danger" title="Error" isInline style={{ marginBottom: '1rem' }}>
          {error}
        </Alert>
      )}
      {successMsg && (
        <Alert variant="success" title="Saved" isInline style={{ marginBottom: '1rem' }}>
          {successMsg}
        </Alert>
      )}

      <Toolbar style={{ marginBottom: '1rem' }}>
        <ToolbarContent>
          <ToolbarItem>
            <SearchInput
              placeholder="Filter packages..."
              value={search}
              onChange={(_e, v) => setSearch(v)}
              onClear={() => setSearch('')}
            />
          </ToolbarItem>
          <ToolbarItem>
            <Button
              variant="secondary"
              onClick={() => setSelected(new Set(upstream.map((p) => p.name)))}
            >
              Select all
            </Button>
          </ToolbarItem>
          <ToolbarItem>
            <Button variant="secondary" onClick={() => setSelected(new Set())}>
              Deselect all
            </Button>
          </ToolbarItem>
          <ToolbarItem align={{ default: 'alignRight' }}>
            <Button
              variant="primary"
              onClick={handleSave}
              isDisabled={!hasChanges || saving}
              isLoading={saving}
            >
              Save changes
            </Button>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      <div style={{ maxHeight: '600px', overflowY: 'auto' }}>
        {visiblePackages.map((pkg) => (
          <div
            key={pkg.name}
            style={{
              display: 'flex',
              alignItems: 'flex-start',
              padding: '0.5rem',
              borderBottom: '1px solid var(--pf-v5-global--BorderColor--100)',
            }}
          >
            <Checkbox
              id={`pkg-${pkg.name}`}
              isChecked={selected.has(pkg.name)}
              onChange={() => togglePackage(pkg.name)}
              label=""
              style={{ marginRight: '0.75rem' }}
            />
            <div>
              <strong>{pkg.name}</strong>
              {pkg.description && (
                <div style={{ fontSize: '0.875rem', color: 'var(--pf-v5-global--Color--200)' }}>
                  {pkg.description}
                </div>
              )}
              <div style={{ fontSize: '0.75rem', color: 'var(--pf-v5-global--Color--300)' }}>
                Default channel: {pkg.defaultChannel}
              </div>
            </div>
          </div>
        ))}
      </div>
    </PageSection>
  );
};

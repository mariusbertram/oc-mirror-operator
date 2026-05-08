import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  Button,
  PageSection,
  SearchInput,
  Spinner,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core';
import { useParams } from 'react-router';
import { Link } from 'react-router-dom';
import {
  getFilteredPackages,
  getPackageConstraints,
  getUpstreamPackages,
  patchCatalogPackages,
} from '../../api/client';
import type { CatalogPackage } from '../../api/types';
import '../../components/plugin-styles.css';

type CatalogBrowserParams = 'targetName' | 'slug' | 'namespace' | 'imageSetName';

type VersionConstraint = { minVersion: string; maxVersion: string };

function sortVersions(versions: string[]): string[] {
  return [...versions].sort((a, b) => {
    const pa = a.split('.').map((s) => parseInt(s, 10) || 0);
    const pb = b.split('.').map((s) => parseInt(s, 10) || 0);
    for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
      const diff = (pa[i] || 0) - (pb[i] || 0);
      if (diff !== 0) return diff;
    }
    return a.localeCompare(b);
  });
}

const versionSelectStyle: React.CSSProperties = {
  fontSize: 11,
  padding: '1px 4px',
  background: 'var(--pf-v6-global--BackgroundColor--100, transparent)',
  color: 'var(--pf-v6-global--Color--100, inherit)',
  border: '1px solid var(--pf-v6-global--BorderColor--100, #d2d2d2)',
  borderRadius: 2,
  maxWidth: 90,
};

export const CatalogBrowser: React.FC = () => {
  const params = useParams<CatalogBrowserParams>();
  let { targetName, slug, namespace, imageSetName } = params;

  if (!targetName) {
    const m = window.location.pathname.match(
      /\/oc-mirror\/targets\/([^/]+)\/namespaces\/([^/]+)\/imagesets\/([^/]+)\/catalogs\/([^/]+)/,
    );
    if (m) {
      targetName = m[1];
      namespace = m[2];
      imageSetName = m[3];
      slug = m[4];
    }
  }

  const [upstream, setUpstream] = useState<CatalogPackage[]>([]);
  const [imported, setImported] = useState<Set<string>>(new Set());
  // versionMap[packageName][channelName] = { minVersion, maxVersion }
  const [versionMap, setVersionMap] = useState<Record<string, Record<string, VersionConstraint>>>({});
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [successMsg, setSuccessMsg] = useState<string | null>(null);

  // UI state
  const [search, setSearch] = useState('');
  const [selectedUp, setSelectedUp] = useState<string | null>(null);
  const [selectedFiltered, setSelectedFiltered] = useState<string | null>(null);
  const [expandedUp, setExpandedUp] = useState<Set<string>>(new Set());
  const [expandedFiltered, setExpandedFiltered] = useState<Set<string>>(new Set());
  const [dirty, setDirty] = useState(false);

  useEffect(() => {
    if (!targetName || !slug || !namespace || !imageSetName) return;
    setLoading(true);
    Promise.all([
      getUpstreamPackages(targetName, slug),
      getFilteredPackages(targetName, slug),
      getPackageConstraints(namespace, imageSetName, slug).catch(() => [] as never[]),
    ])
      .then(([upResp, fpResp, constraints]) => {
        setUpstream(upResp.packages);
        setImported(new Set(fpResp.packages.map((p) => p.name)));
        const vm: Record<string, Record<string, VersionConstraint>> = {};
        for (const pkg of constraints) {
          for (const ch of pkg.channels || []) {
            if (ch.minVersion || ch.maxVersion) {
              if (!vm[pkg.name]) vm[pkg.name] = {};
              vm[pkg.name][ch.name] = {
                minVersion: ch.minVersion || '',
                maxVersion: ch.maxVersion || '',
              };
            }
          }
        }
        setVersionMap(vm);
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  }, [targetName, slug, namespace, imageSetName]);

  const visibleUpstream = useMemo(
    () =>
      upstream.filter(
        (p) =>
          !search ||
          p.name.toLowerCase().includes(search.toLowerCase()) ||
          (p.description || '').toLowerCase().includes(search.toLowerCase()),
      ),
    [upstream, search],
  );

  const importPackage = (name: string) => {
    setImported((prev) => new Set([...prev, name]));
    setDirty(true);
  };

  const removePackage = (name: string) => {
    setImported((prev) => {
      const next = new Set(prev);
      next.delete(name);
      return next;
    });
    if (selectedFiltered === name) setSelectedFiltered(null);
    setDirty(true);
  };

  const importAll = () => {
    setImported(new Set(upstream.map((p) => p.name)));
    setDirty(true);
  };

  const removeAll = () => {
    setImported(new Set());
    setSelectedFiltered(null);
    setDirty(true);
  };

  const setVersionConstraint = (
    pkgName: string,
    channelName: string,
    field: 'minVersion' | 'maxVersion',
    value: string,
  ) => {
    setVersionMap((prev) => {
      const pkg = prev[pkgName] || {};
      const ch = pkg[channelName] || { minVersion: '', maxVersion: '' };
      return { ...prev, [pkgName]: { ...pkg, [channelName]: { ...ch, [field]: value } } };
    });
    setDirty(true);
  };

  const handleSave = async () => {
    if (!namespace || !imageSetName || !slug) return;
    setSaving(true);
    setError(null);
    setSuccessMsg(null);
    const packages = importedPackages.map((p) => {
      const pkgConstraints = versionMap[p.name] || {};
      const channels = p.channels
        .filter((c) => pkgConstraints[c.name]?.minVersion || pkgConstraints[c.name]?.maxVersion)
        .map((c) => ({
          name: c.name,
          minVersion: pkgConstraints[c.name]?.minVersion || undefined,
          maxVersion: pkgConstraints[c.name]?.maxVersion || undefined,
        }));
      return { name: p.name, channels };
    });
    const exclude = upstream.filter((p) => !imported.has(p.name)).map((p) => p.name);
    try {
      await patchCatalogPackages(namespace, imageSetName, slug, { packages, exclude });
      setDirty(false);
      setSuccessMsg(`Saved — ${packages.length} packages included, ${exclude.length} excluded.`);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const toggleExpandUp = (name: string) => {
    setExpandedUp((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  };

  const toggleExpandFiltered = (name: string) => {
    setExpandedFiltered((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  };

  if (loading) return <PageSection><Spinner /></PageSection>;

  const importedPackages = upstream.filter((p) => imported.has(p.name));

  const selectedUpPkg = upstream.find((p) => p.name === selectedUp);
  const selectedFiltPkg = importedPackages.find((p) => p.name === selectedFiltered);

  return (
    <>
      <PageSection style={{ paddingBottom: 0, borderBottom: '1px solid var(--pf-v6-global--BorderColor--100)' }}>
        <div style={{ marginBottom: 6 }}>
          <Link to={`/oc-mirror/targets/${targetName}`} style={{ fontSize: 13 }}>
            ← Back to {targetName}
          </Link>
        </div>
        <div className="mirror-row" style={{ marginBottom: 8 }}>
          <Title headingLevel="h1">Catalog browser — {slug}</Title>
          <div className="mirror-spacer" />
          {dirty && (
            <Button
              variant="primary"
              onClick={handleSave}
              isDisabled={saving}
              isLoading={saving}
            >
              Save changes
            </Button>
          )}
        </div>
        <p style={{ margin: '0 0 12px', color: 'var(--pf-v6-global--Color--200)', fontSize: 13 }}>
          Import packages from <strong>{slug}</strong> into ImageSet{' '}
          <span className="mirror-tag">{imageSetName}</span>
        </p>
      </PageSection>

      <PageSection>
        {error && (
          <Alert variant="danger" title="Error" isInline style={{ marginBottom: 12 }}>{error}</Alert>
        )}
        {successMsg && (
          <Alert variant="success" title="Saved" isInline style={{ marginBottom: 12 }}>{successMsg}</Alert>
        )}

        <div className="mirror-dual">
          {/* ── Upstream pane ── */}
          <div className="mirror-dual-pane">
            <div className="mirror-dual-pane__header">
              <h3>Upstream catalog</h3>
              <span style={{ fontSize: 12, color: 'var(--pf-v6-global--Color--200)' }}>
                {visibleUpstream.length} packages
              </span>
            </div>
            <div style={{ padding: '8px 12px', borderBottom: '1px solid var(--pf-v6-global--BorderColor--100)' }}>
              <SearchInput
                placeholder="Filter packages…"
                value={search}
                onChange={(_e, v) => setSearch(v)}
                onClear={() => setSearch('')}
              />
            </div>
            <div className="mirror-dual-pane__body">
              {visibleUpstream.map((p) => {
                const isImported = imported.has(p.name);
                const expanded = expandedUp.has(p.name);
                return (
                  <React.Fragment key={p.name}>
                    <div
                      className={`mirror-dual-row${selectedUp === p.name ? ' mirror-dual-row--selected' : ''}`}
                      onClick={() => setSelectedUp(p.name)}
                    >
                      <button
                        style={{
                          background: 'none',
                          border: 'none',
                          cursor: 'pointer',
                          padding: 0,
                          fontSize: 12,
                          color: 'var(--pf-v6-global--Color--200)',
                          transform: expanded ? 'rotate(90deg)' : 'none',
                          transition: 'transform 100ms',
                          width: 20,
                          height: 20,
                          display: 'grid',
                          placeItems: 'center',
                        }}
                        onClick={(e) => { e.stopPropagation(); toggleExpandUp(p.name); }}
                        aria-label={expanded ? 'Collapse' : 'Expand'}
                      >
                        ▶
                      </button>
                      <div>
                        <div className="mirror-dual-row__name">
                          {p.name}
                          {isImported && (
                            <span className="mirror-tag" style={{ marginLeft: 6, color: 'var(--pf-v6-global--success-color--100)', borderColor: 'var(--pf-v6-global--success-color--100)' }}>
                              imported
                            </span>
                          )}
                        </div>
                        <div className="mirror-dual-row__meta">
                          {p.channels.length} channels · default: <code style={{ fontSize: 10 }}>{p.defaultChannel}</code>
                        </div>
                      </div>
                      <Button
                        variant="secondary"
                        size="sm"
                        isDisabled={isImported}
                        onClick={(e) => { e.stopPropagation(); importPackage(p.name); }}
                        title={isImported ? 'Already imported' : 'Import package'}
                      >
                        Import
                      </Button>
                    </div>
                    {expanded && p.channels.map((c) => {
                      const uniqueVersions = sortVersions([...new Set(c.entries.map((e) => e.version))]);
                      const displayVersions = uniqueVersions.length > 5
                        ? `${uniqueVersions.slice(0, 5).join(', ')} +${uniqueVersions.length - 5} more`
                        : uniqueVersions.join(', ') || `${c.entries.length} entries`;
                      return (
                        <div key={c.name} className="mirror-dual-channel">
                          <span className="mirror-dual-channel__dot" />
                          <div>
                            <div style={{ fontWeight: 500 }}>{c.name}</div>
                            <div style={{ color: 'var(--pf-v6-global--Color--200)', fontSize: 10 }}>
                              {displayVersions}
                            </div>
                          </div>
                          <Button
                            variant="link"
                            size="sm"
                            isDisabled={isImported}
                            style={{ paddingLeft: 0 }}
                            onClick={() => importPackage(p.name)}
                          >
                            {isImported ? 'added' : '+ add'}
                          </Button>
                        </div>
                      );
                    })}
                  </React.Fragment>
                );
              })}
            </div>
            {selectedUpPkg && (
              <div className="mirror-dual-pane__footer">
                <strong>{selectedUpPkg.name}</strong>
                {selectedUpPkg.description && (
                  <div style={{ marginTop: 4 }}>{selectedUpPkg.description}</div>
                )}
                <dl className="mirror-kv" style={{ marginTop: 6, fontSize: 12 }}>
                  <dt>Default channel</dt><dd><code style={{ fontSize: 11 }}>{selectedUpPkg.defaultChannel}</code></dd>
                  <dt>Channels</dt><dd>{selectedUpPkg.channels.length}</dd>
                </dl>
              </div>
            )}
          </div>

          {/* ── Action buttons ── */}
          <div className="mirror-dual-actions">
            <button
              className="mirror-dual-action-btn"
              disabled={!selectedUp || imported.has(selectedUp)}
              onClick={() => selectedUp && importPackage(selectedUp)}
              title="Import selected →"
            >
              ›
            </button>
            <button
              className="mirror-dual-action-btn"
              onClick={importAll}
              title="Import all »"
            >
              »
            </button>
            <div className="mirror-dual-spacer" />
            <button
              className="mirror-dual-action-btn"
              disabled={!selectedFiltered}
              onClick={() => selectedFiltered && removePackage(selectedFiltered)}
              title="← Remove selected"
            >
              ‹
            </button>
            <button
              className="mirror-dual-action-btn"
              disabled={imported.size === 0}
              onClick={removeAll}
              title="«Remove all"
            >
              «
            </button>
          </div>

          {/* ── Filtered / imported pane ── */}
          <div className="mirror-dual-pane">
            <div className="mirror-dual-pane__header">
              <h3>
                In ImageSet <code style={{ fontSize: 11, marginLeft: 4 }}>{imageSetName}</code>
              </h3>
              <span style={{ fontSize: 12, color: 'var(--pf-v6-global--Color--200)' }}>
                {importedPackages.length} packages
              </span>
            </div>
            <div className="mirror-dual-pane__body">
              {importedPackages.length === 0 && (
                <div style={{ padding: 32, textAlign: 'center', color: 'var(--pf-v6-global--Color--200)', fontSize: 13 }}>
                  No packages imported yet. Select a package on the left and click Import.
                </div>
              )}
              {importedPackages.map((p) => {
                const expanded = expandedFiltered.has(p.name);
                return (
                  <React.Fragment key={p.name}>
                    <div
                      className={`mirror-dual-row${selectedFiltered === p.name ? ' mirror-dual-row--selected' : ''}`}
                      onClick={() => setSelectedFiltered(p.name)}
                    >
                      <button
                        style={{
                          background: 'none',
                          border: 'none',
                          cursor: 'pointer',
                          padding: 0,
                          fontSize: 12,
                          color: 'var(--pf-v6-global--Color--200)',
                          transform: expanded ? 'rotate(90deg)' : 'none',
                          transition: 'transform 100ms',
                          width: 20,
                          height: 20,
                          display: 'grid',
                          placeItems: 'center',
                        }}
                        onClick={(e) => { e.stopPropagation(); toggleExpandFiltered(p.name); }}
                        aria-label={expanded ? 'Collapse' : 'Expand'}
                      >
                        ▶
                      </button>
                      <div>
                        <div className="mirror-dual-row__name">{p.name}</div>
                        <div className="mirror-dual-row__meta">
                          {p.channels.length} channels · default: <code style={{ fontSize: 10 }}>{p.defaultChannel}</code>
                        </div>
                      </div>
                      <button
                        style={{
                          background: 'none',
                          border: 'none',
                          cursor: 'pointer',
                          padding: '0 4px',
                          fontSize: 16,
                          color: 'var(--pf-v6-global--danger-color--100)',
                          lineHeight: 1,
                        }}
                        onClick={(e) => { e.stopPropagation(); removePackage(p.name); }}
                        title="Remove"
                      >
                        ×
                      </button>
                    </div>
                    {expanded && p.channels.map((c) => {
                      const uniqueVersions = sortVersions([...new Set(c.entries.map((e) => e.version))]);
                      const constraint = versionMap[p.name]?.[c.name] || { minVersion: '', maxVersion: '' };
                      return (
                        <div key={c.name} className="mirror-dual-channel" style={{ gridTemplateColumns: '20px 1fr auto' }}>
                          <span className="mirror-dual-channel__dot mirror-dual-channel__dot--imported" />
                          <div>
                            <div style={{ fontWeight: 500 }}>{c.name}</div>
                            <div style={{ color: 'var(--pf-v6-global--Color--200)', fontSize: 10 }}>
                              {uniqueVersions.length} versions
                            </div>
                          </div>
                          <div
                            style={{ display: 'flex', gap: 4, alignItems: 'center' }}
                            onClick={(e) => e.stopPropagation()}
                          >
                            <label style={{ fontSize: 10, color: 'var(--pf-v6-global--Color--200)', whiteSpace: 'nowrap' }}>Min</label>
                            <select
                              value={constraint.minVersion}
                              onChange={(e) => setVersionConstraint(p.name, c.name, 'minVersion', e.target.value)}
                              style={versionSelectStyle}
                              title="Minimum version (inclusive)"
                            >
                              <option value="">any</option>
                              {uniqueVersions.map((v) => <option key={v} value={v}>{v}</option>)}
                            </select>
                            <label style={{ fontSize: 10, color: 'var(--pf-v6-global--Color--200)', whiteSpace: 'nowrap' }}>Max</label>
                            <select
                              value={constraint.maxVersion}
                              onChange={(e) => setVersionConstraint(p.name, c.name, 'maxVersion', e.target.value)}
                              style={versionSelectStyle}
                              title="Maximum version (inclusive)"
                            >
                              <option value="">any</option>
                              {uniqueVersions.map((v) => <option key={v} value={v}>{v}</option>)}
                            </select>
                          </div>
                        </div>
                      );
                    })}
                  </React.Fragment>
                );
              })}
            </div>
            {selectedFiltPkg && (
              <div className="mirror-dual-pane__footer">
                <strong>{selectedFiltPkg.name}</strong>
                <dl className="mirror-kv" style={{ marginTop: 6, fontSize: 12 }}>
                  <dt>Default channel</dt><dd><code style={{ fontSize: 11 }}>{selectedFiltPkg.defaultChannel}</code></dd>
                  <dt>Channels</dt><dd>{selectedFiltPkg.channels.length}</dd>
                </dl>
              </div>
            )}
          </div>
        </div>

        {dirty && (
          <div style={{ marginTop: 16, display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
            <Button variant="tertiary" onClick={() => { setDirty(false); }}>Cancel</Button>
            <Button variant="primary" onClick={handleSave} isDisabled={saving} isLoading={saving}>
              Save changes
            </Button>
          </div>
        )}
      </PageSection>
    </>
  );
};

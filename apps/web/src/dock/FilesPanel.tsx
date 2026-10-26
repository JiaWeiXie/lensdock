import { useCallback, useEffect, useRef, useState, type DragEvent, type ReactNode } from 'react';
import type { ApiError } from '@/api/http';
import { isAbortError } from '@/util/async';
import type { FileArea, FileListing } from '@/api/types';
import { deleteFile, fileDownloadUrl, listFiles, uploadFile } from '@/api/workspace';
import { Button, Meter, Modal, Notice, Tabs, tabPanelId } from '@/components/ui';
import { describeError } from '@/errors';
import { useI18n } from '@/i18n';
import { canOperate, useSession } from '@/state/session';
import { useWorkspace } from '@/state/workspace';
import { formatBytes, formatDateTime } from '@/util/format';
import { utf8ByteLength } from '@/util/text';

/** Mirrors the runtime name rules so most mistakes are caught before the upload starts. */
export function fileNameProblem(name: string): 'empty' | 'long' | 'chars' | 'dot' | null {
  if (name.length === 0) return 'empty';
  if (utf8ByteLength(name) > 255) return 'long';
  if (name === '.' || name === '..' || name.startsWith('.')) return 'dot';
  if (/[/\\\u0000-\u001f\u007f]/.test(name)) return 'chars';
  return null;
}

interface UploadState {
  name: string;
  loaded: number;
  total: number;
}

/** Controlled file transfer: uploads land in the runtime uploads dir, downloads only on an explicit click. */
export function FilesPanel(): ReactNode {
  const { t, locale } = useI18n();
  const { actor } = useSession();
  const { workspaceId, eventTick } = useWorkspace();
  const [area, setArea] = useState<FileArea>('uploads');
  const [listing, setListing] = useState<FileListing | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [upload, setUpload] = useState<UploadState | null>(null);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const [overwrite, setOverwrite] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const abort = useRef<AbortController | null>(null);
  const operator = canOperate(actor?.role);

  const refresh = useCallback(async (): Promise<void> => {
    try {
      setListing(await listFiles(workspaceId, area));
      setError(null);
    } catch (err) {
      setError(err as ApiError);
    }
  }, [workspaceId, area]);

  useEffect(() => {
    if (operator) void refresh();
  }, [refresh, operator, eventTick]);

  const startUpload = async (file: File): Promise<void> => {
    setUploadError(null);
    const problem = fileNameProblem(file.name);
    if (problem) {
      setUploadError(t(`files.name.${problem}`));
      return;
    }
    const usage = listing?.usage;
    if (usage && usage.quotaBytes > 0 && file.size > Math.max(0, usage.quotaBytes - usage.usedBytes)) {
      setUploadError(t('files.quotaWouldExceed', { size: formatBytes(file.size), free: formatBytes(Math.max(0, usage.quotaBytes - usage.usedBytes)) }));
      return;
    }
    if (usage && usage.maxFiles > 0 && usage.fileCount >= usage.maxFiles && !listing?.files.some((f) => f.name === file.name)) {
      setUploadError(t('files.tooManyFiles', { max: usage.maxFiles }));
      return;
    }
    const exists = area === 'uploads' && listing?.files.some((f) => f.name === file.name);
    if (exists && !overwrite) {
      setUploadError(t('files.exists', { name: file.name }));
      return;
    }
    const controller = new AbortController();
    abort.current = controller;
    setUpload({ name: file.name, loaded: 0, total: file.size });
    try {
      await uploadFile(workspaceId, file, {
        overwrite,
        signal: controller.signal,
        onProgress: (loaded, total) => setUpload({ name: file.name, loaded, total }),
      });
      await refresh();
    } catch (err) {
      if (!isAbortError(err)) setUploadError(describeError(err, t));
    } finally {
      setUpload(null);
      abort.current = null;
    }
  };

  const onDrop = (e: DragEvent<HTMLDivElement>): void => {
    e.preventDefault();
    setDragging(false);
    const file = e.dataTransfer.files[0];
    if (file && area === 'uploads' && operator) void startUpload(file);
  };

  if (!operator) {
    return (
      <section className="panel">
        <h3>{t('files.title')}</h3>
        <Notice tone="neutral">{t('files.needHuman')}</Notice>
      </section>
    );
  }

  const usage = listing?.usage;

  return (
    <section className="panel">
      <h3>{t('files.title')}</h3>
      <p className="panel__desc">{t('files.desc')}</p>
      <Tabs
        group="files"
        label={t('files.areas')}
        value={area}
        onChange={setArea}
        items={[
          { id: 'uploads', label: t('files.area.uploads') },
          { id: 'downloads', label: t('files.area.downloads') },
        ]}
      />
      <div role="tabpanel" id={tabPanelId('files', area)} aria-labelledby={`files-tab-${area}`}>

      {usage ? (
        <div className="quota">
          {usage.quotaBytes > 0 ? <Meter value={usage.usedBytes} max={usage.quotaBytes} label={t('files.quota')} /> : null}
          <p className="mono">
            {formatBytes(usage.usedBytes)}
            {usage.quotaBytes > 0 ? ` / ${formatBytes(usage.quotaBytes)}` : ''} ·{' '}
            {usage.maxFiles > 0 ? t('files.count', { n: usage.fileCount, max: usage.maxFiles }) : t('files.countOnly', { n: usage.fileCount })}
          </p>
        </div>
      ) : null}
      {error ? <Notice tone="danger">{describeError(error, t)}</Notice> : null}

      {area === 'uploads' ? (
        <div
          className={`dropzone ${dragging ? 'is-over' : ''}`}
          onDragOver={(e) => {
            e.preventDefault();
            setDragging(true);
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={onDrop}
        >
          <p>{t('files.drop')}</p>
          <input
            id="file-input"
            type="file"
            className="sr-only"
            disabled={upload !== null}
            onChange={(e) => {
              const f = e.target.files?.[0];
              e.target.value = '';
              if (f) void startUpload(f);
            }}
          />
          <label htmlFor="file-input" className={`btn btn--secondary btn--sm ${upload ? 'is-busy' : ''}`}>
            <span className="btn__label">{t('files.choose')}</span>
          </label>
          <label className="check">
            <input type="checkbox" checked={overwrite} onChange={(e) => setOverwrite(e.target.checked)} />
            <span>{t('files.overwrite')}</span>
          </label>
        </div>
      ) : null}

      {upload ? (
        <div className="upload" role="status">
          <p className="mono">
            {upload.name} · {formatBytes(upload.loaded)} / {formatBytes(upload.total)}
          </p>
          <Meter value={upload.loaded} max={upload.total} label={t('files.uploading')} />
          <Button small variant="warn" onClick={() => abort.current?.abort()}>
            {t('common.cancel')}
          </Button>
        </div>
      ) : null}
      {uploadError ? <Notice tone="danger">{uploadError}</Notice> : null}

      {listing && listing.files.length === 0 ? <p className="empty">{t('files.empty')}</p> : null}
      <ul className="rows">
        {listing?.files.map((f) => (
          <li key={f.name} className="row">
            <div className="row__main">
              <strong className="row__name">{f.name}</strong>
              <span className="row__meta mono">
                {formatBytes(f.size)} · {formatDateTime(f.modifiedAt, locale)}
              </span>
            </div>
            <div className="row__btns">
              <a className="btn btn--secondary btn--sm" href={fileDownloadUrl(workspaceId, area, f.name)} download={f.name}>
                <span className="btn__label">{t('files.download')}</span>
              </a>
              <Button small variant="ghost" icon="trash" aria-label={t('files.delete')} onClick={() => setConfirmDelete(f.name)} />
            </div>
          </li>
        ))}
      </ul>

      </div>

      <Modal
        open={confirmDelete !== null}
        onClose={() => setConfirmDelete(null)}
        title={t('files.deleteTitle')}
        footer={
          <>
            <Button onClick={() => setConfirmDelete(null)}>{t('common.cancel')}</Button>
            <Button
              variant="danger"
              icon="trash"
              onClick={() => {
                const name = confirmDelete;
                if (!name) return;
                void deleteFile(workspaceId, area, name)
                  .then(refresh)
                  .catch((err: unknown) => setUploadError(describeError(err, t)))
                  .finally(() => setConfirmDelete(null));
              }}
            >
              {t('files.delete')}
            </Button>
          </>
        }
      >
        <p>{t('files.deleteBody', { name: confirmDelete ?? '' })}</p>
      </Modal>
    </section>
  );
}

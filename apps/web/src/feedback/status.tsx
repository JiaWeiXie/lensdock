import type { ReactNode } from 'react';
import type { DeliveryState } from '@/api/types';
import { Chip, type Tone } from '@/components/ui';
import type { IconName } from '@/components/icons';
import { useI18n, type MessageKey } from '@/i18n';

/**
 * Delivery states are deliberately distinct: "published", "queued" and
 * "acknowledged" are different facts and never collapse into one "sent".
 */
export const DELIVERY_ORDER: readonly DeliveryState[] = ['READY', 'QUEUED', 'ACKNOWLEDGED', 'IN_PROGRESS', 'RESOLVED', 'VERIFIED'];

export const DELIVERY_LABEL: Record<DeliveryState, MessageKey> = {
  READY: 'delivery.READY',
  QUEUED: 'delivery.QUEUED',
  ACKNOWLEDGED: 'delivery.ACKNOWLEDGED',
  IN_PROGRESS: 'delivery.IN_PROGRESS',
  RESOLVED: 'delivery.RESOLVED',
  VERIFIED: 'delivery.VERIFIED',
  CANCELLED: 'delivery.CANCELLED',
};

const TONE: Record<DeliveryState, Tone> = {
  READY: 'neutral',
  QUEUED: 'amber',
  ACKNOWLEDGED: 'agent',
  IN_PROGRESS: 'teal',
  RESOLVED: 'amber',
  VERIFIED: 'ok',
  CANCELLED: 'danger',
};

const ICON: Record<DeliveryState, IconName> = {
  READY: 'file',
  QUEUED: 'send',
  ACKNOWLEDGED: 'eye',
  IN_PROGRESS: 'activity',
  RESOLVED: 'warning',
  VERIFIED: 'check',
  CANCELLED: 'x',
};

export function DeliveryChip({ state }: { state: DeliveryState }): ReactNode {
  const { t } = useI18n();
  return (
    <Chip tone={TONE[state]} icon={ICON[state]}>
      {t(DELIVERY_LABEL[state])}
    </Chip>
  );
}

export const TERMINAL: readonly DeliveryState[] = ['VERIFIED', 'CANCELLED'];

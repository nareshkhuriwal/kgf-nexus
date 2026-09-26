// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Notifications: channels (where), rules (when), deliveries (what happened).
 *
 * The tab is URL-addressed (`?tab=deliveries&state=dead`) so the dead-letter
 * view is a link an operator can paste into an incident channel.
 */

import { useSearchParams } from 'react-router-dom';
import { PageHeader } from '../components/PageHeader';
import { Forbidden } from '../components/Forbidden';
import { usePermission } from '../components/Permissions';
import { ChannelsTab } from './NotificationChannels';
import { RulesTab } from './NotificationRules';
import { DeliveriesTab } from './NotificationDeliveries';

const TABS = [
  ['channels', 'Channels'],
  ['rules', 'Rules'],
  ['deliveries', 'Deliveries'],
] as const;
type Tab = (typeof TABS)[number][0];

export function NotificationsPage({ namespace }: { namespace: string }) {
  const can = usePermission();
  const [params, setParams] = useSearchParams();
  const raw = params.get('tab');
  const tab: Tab = TABS.some(([id]) => id === raw) ? (raw as Tab) : 'channels';

  if (!can('notifications.view')) {
    return (
      <div className="col">
        <PageHeader title="Notifications" />
        <Forbidden
          what="view notifications"
          detail="This page needs the notifications.view permission. Ask your administrator to add it to one of your roles."
        />
      </div>
    );
  }

  return (
    <div className="col">
      <PageHeader
        title="Notifications"
        subtitle={
          <>
            where run events are sent in <code>{namespace}</code>
            {!can('notifications.manage') && ' · read-only: changing these needs notifications.manage'}
          </>
        }
      />
      <div className="cluster-tabs" role="group" aria-label="Notification sections">
        {TABS.map(([id, label]) => (
          <button
            key={id}
            type="button"
            aria-pressed={tab === id}
            onClick={() => setParams(id === 'channels' ? {} : { tab: id }, { replace: true })}
          >
            {label}
          </button>
        ))}
      </div>
      {tab === 'channels' && <ChannelsTab key={namespace} namespace={namespace} />}
      {tab === 'rules' && <RulesTab key={namespace} namespace={namespace} />}
      {tab === 'deliveries' && <DeliveriesTab key={namespace} namespace={namespace} />}
    </div>
  );
}

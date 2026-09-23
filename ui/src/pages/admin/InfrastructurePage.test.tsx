// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { InfrastructurePage } from './InfrastructurePage';
import { PermissionContext } from '../../components/Permissions';
import { authRequest } from '../../api/auth';
import { api } from '../../api/client';
vi.mock('../../api/auth',()=>({authRequest:vi.fn()}));
vi.mock('../../api/client',()=>({api:{listWorkers:vi.fn(),getWorker:vi.fn()}}));
afterEach(()=>{cleanup();vi.resetAllMocks()});
function setup(grants=['infrastructure.view','infrastructure.configure','workers.view']) {
 vi.mocked(authRequest).mockImplementation(async path=>path.endsWith('/config')?{revision:1,scheduler:{min_replicas:1,max_replicas:4,target_cpu_percent:65,scale_down_seconds:300},profiles:[],pools:[]}:{revision:1,decisions:[],task_job_templates:{}} as never);
 vi.mocked(api.listWorkers).mockResolvedValue([{id:'worker-1',hostname:'test-host',namespace:'default',state:'ACTIVE',queues:['default'],capacity:{cpu_millis:2000},last_heartbeat_at:new Date().toISOString(),active_attempts:1}]);
 vi.mocked(api.getWorker).mockResolvedValue({id:'worker-1',hostname:'test-host',running_attempts:[{id:'attempt-1',node_id:'extract',run_id:'run-1',task_ref:'etl.extract',attempt_number:1,state:'RUNNING'}]} as never);
 render(<MemoryRouter><PermissionContext.Provider value={grants}><QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><InfrastructurePage namespace="default" canAdmin/></QueryClientProvider></PermissionContext.Provider></MemoryRouter>);
}
it('shows real agents and loads task placement only after inspecting an agent',async()=>{
 setup();expect(await screen.findByText('test-host')).toBeTruthy();expect(api.getWorker).not.toHaveBeenCalled();
 fireEvent.click(screen.getByRole('button',{name:'Inspect tasks'}));expect(await screen.findByText('etl.extract')).toBeTruthy();
 expect(screen.getByRole('link',{name:'View run and task details →'}).getAttribute('href')).toBe('/runs/run-1');
 expect(api.getWorker).toHaveBeenCalledWith('default','worker-1');
});
it('separates scheduler policy from live status and exposes its controls',async()=>{
 setup();await screen.findByText('test-host');fireEvent.click(screen.getByRole('button',{name:'Scheduler'}));
 expect(screen.getByRole('spinbutton',{name:'Minimum replicas'})).toHaveValue(1);
 expect(screen.getByRole('spinbutton',{name:'Maximum replicas'})).toHaveValue(4);
 expect(screen.queryByRole('button',{name:'Inspect tasks'})).toBeNull();
});
it('does not fetch worker inventory or expose save for read-only infrastructure access',async()=>{
 setup(['infrastructure.view']);await screen.findByText(/Worker inventory requires/);
 expect(api.listWorkers).not.toHaveBeenCalled();expect(screen.queryByRole('button',{name:'Save configuration'})).toBeNull();
});
it('keeps linked pools synchronized when a profile is renamed and protects referenced profiles',async()=>{
 setup();await screen.findByText('test-host');
 fireEvent.click(screen.getByRole('button',{name:'Task runtime'}));
 fireEvent.click(screen.getByRole('button',{name:'Add pod profile'}));
 fireEvent.click(screen.getByRole('button',{name:'Worker pools'}));
 fireEvent.click(screen.getByRole('button',{name:'Add worker pool'}));
 expect(screen.getByRole('combobox',{name:'Pod profile'})).toHaveValue('profile-1');
 fireEvent.click(screen.getByRole('button',{name:'Task runtime'}));
 fireEvent.change(screen.getByLabelText('Profile name'),{target:{value:'aks-dev-small'}});
 expect(screen.getByRole('button',{name:'Remove profile'})).toBeDisabled();
 fireEvent.click(screen.getByRole('button',{name:'Worker pools'}));
 expect(screen.getByRole('combobox',{name:'Pod profile'})).toHaveValue('aks-dev-small');
});
it('shows the fair-share weight, defaulting to 1 for a policy saved before the field existed',async()=>{
 setup();await screen.findByText('test-host');fireEvent.click(screen.getByRole('button',{name:'Scheduler'}));
 expect(screen.getByRole('spinbutton',{name:'Fair-share weight'})).toHaveValue(1);
 expect(screen.getByText(/Changing the weight requires a platform operator/)).toBeTruthy();
});
it('shows a stored fair-share weight',async()=>{
 vi.mocked(authRequest).mockImplementation(async path=>path.endsWith('/config')?{revision:2,scheduler:{min_replicas:1,max_replicas:4,target_cpu_percent:65,scale_down_seconds:300,fair_share_weight:3},profiles:[],pools:[]}:{revision:2,decisions:[],task_job_templates:{}} as never);
 vi.mocked(api.listWorkers).mockResolvedValue([]);
 render(<MemoryRouter><PermissionContext.Provider value={['infrastructure.view','infrastructure.configure']}><QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><InfrastructurePage namespace="default" canAdmin/></QueryClientProvider></PermissionContext.Provider></MemoryRouter>);
 fireEvent.click(await screen.findByRole('button',{name:'Scheduler'}));
 expect(await screen.findByRole('spinbutton',{name:'Fair-share weight'})).toHaveValue(3);
});

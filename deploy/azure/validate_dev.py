"""Provision the explicitly scoped dev AKS, smoke test it, and always request stop.
Run manually; never schedules automatic starts. Budget is tracking, not a hard cap.
"""
import ipaddress, json, subprocess, tempfile, urllib.request
SUB = 'a136dc2d-6000-426d-843c-1f8b12d597fb'
RG = 'kgf-nexus-dev-cin-rg'
CLUSTER = 'kgf-nexus-dev-cin-aks'

def az(*args, timeout=1800):
    return subprocess.run(['az', *args, '--subscription', SUB, '-o', 'json'], check=True, capture_output=True, text=True, timeout=timeout)

def show():
    return json.loads(az('aks', 'show', '-g', RG, '-n', CLUSTER).stdout)

if __name__ == '__main__':
    existing = json.loads(az('aks', 'list', '-g', RG).stdout)
    if existing:
        raise SystemExit('Cluster already present; inspect it before re-running this provisioning script.')
    public_ip = str(ipaddress.ip_address(urllib.request.urlopen('https://api.ipify.org', timeout=20).read().decode().strip()))
    try:
        print('Creating two fixed D4s_v3 dev nodes, Free control plane; no autoscaling.', flush=True)
        az('aks', 'create', '-g', RG, '-n', CLUSTER, '--location', 'centralindia',
           '--node-resource-group', 'kgf-nexus-dev-cin-nodes-rg', '--tier', 'free',
           '--kubernetes-version', '1.35.7', '--node-count', '2', '--node-vm-size', 'Standard_D4s_v3',
           '--node-osdisk-size', '32', '--node-osdisk-type', 'Managed', '--os-sku', 'Ubuntu',
           '--network-plugin', 'azure', '--network-plugin-mode', 'overlay', '--network-dataplane', 'cilium',
           '--enable-managed-identity', '--enable-oidc-issuer', '--enable-workload-identity',
           '--no-ssh-key', '--api-server-authorized-ip-ranges', public_ip + '/32',
           '--tags', 'project=kgf-nexus', 'environment=dev', 'budget-usd=50')
        print('Cluster created. Checking Kubernetes nodes and a disposable test job.', flush=True)
        with tempfile.TemporaryDirectory(prefix='nexus-aks-') as tmp:
            cfg = tmp + '/config'
            az('aks', 'get-credentials', '-g', RG, '-n', CLUSTER, '--file', cfg)
            def kube(*args):
                subprocess.run(['kubectl', '--kubeconfig', cfg, *args], check=True, timeout=180)
            kube('wait', '--for=condition=Ready', 'nodes', '--all', '--timeout=120s')
            kube('get', 'nodes', '-o', 'wide')
            kube('create', 'namespace', 'nexus-validation')
            try:
                kube('-n', 'nexus-validation', 'create', 'job', 'smoke', '--image=busybox:1.37.0', '--', 'sh', '-c', 'echo nexus-aks-smoke-ok')
                kube('-n', 'nexus-validation', 'wait', '--for=condition=complete', 'job/smoke', '--timeout=120s')
                kube('-n', 'nexus-validation', 'logs', 'job/smoke')
            finally:
                kube('delete', 'namespace', 'nexus-validation', '--wait=false')
    except subprocess.CalledProcessError as error:
        print(error.stderr or str(error), flush=True)
        raise
    finally:
        clusters = json.loads(az('aks', 'list', '-g', RG).stdout)
        if any(c['name'] == CLUSTER for c in clusters):
            print('Requesting AKS stop to end compute billing.', flush=True)
            az('aks', 'stop', '-g', RG, '-n', CLUSTER)
            state = show()
            print(json.dumps({'name': state['name'], 'powerState': state.get('powerState'), 'provisioningState': state.get('provisioningState')}), flush=True)
            if state.get('powerState', {}).get('code') != 'Stopped':
                raise RuntimeError('STOP NOT VERIFIED: inspect cluster immediately.')

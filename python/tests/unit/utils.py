from tests.utils import EnvoyHCMInfo, default_listener_manifests

def module_and_mapping_manifests(module_confs, mapping_confs):
    yaml = default_listener_manifests() + """
---
apiVersion: getambassador.io/v3alpha1
kind: Module
metadata:
  name: ambassador
  namespace: default
spec:
  config:"""
    if module_confs:
        for module_conf in module_confs:
            yaml = yaml + """
    {}
""".format(module_conf)
    else:
        yaml = yaml + " {}\n"

    yaml = yaml + """
---
apiVersion: getambassador.io/v3alpha1
kind: Mapping
metadata:
  name: ambassador
  namespace: default
spec:
  hostname: "*"
  prefix: /httpbin/
  service: httpbin"""
    if mapping_confs:
        for mapping_conf in mapping_confs:
            yaml = yaml + """
  {}""".format(mapping_conf)
    return yaml

def econf_foreach_listener(econf, fn, listener_count=1):
    listeners = econf['static_resources']['listeners']

    wanted_plural = "" if (listener_count == 1) else "s"
    assert len(listeners) == listener_count, f"Expected {listener_count} listener{wanted_plural}, got {len(listeners)}"

    for listener in listeners:
        fn(listener)

def econf_foreach_listener_chain(listener, fn, chain_count=2, need_name=None, need_type=None, dump_info=None):
    # We need a specific number of filter chains. Normally it's 2,
    # since the compiler tests don't generally supply Listeners or Hosts,
    # so we get secure and insecure chains.
    filter_chains = listener['filter_chains']

    if dump_info:
        dump_info(filter_chains)

    wanted_plural = "" if (chain_count == 1) else "s"
    assert len(filter_chains) == chain_count, f"Expected {chain_count} filter chain{wanted_plural}, got {len(filter_chains)}"

    for chain in filter_chains:
        # We expect one filter on this chain.
        filters = chain['filters']
        got_count = len(filters)
        got_plural = "" if (got_count == 1) else "s"
        assert got_count == 1, f"Expected just one filter, got {got_count} filter{got_plural}"

        # The http connection manager is the only filter on the chain from the one and only vhost.
        filter = filters[0]

        if need_name:
            assert filter['name'] == need_name

        typed_config = filter['typed_config']

        if need_type:
            assert typed_config['@type'] == need_type, f"bad type: got {repr(typed_config['@type'])} but expected {repr(need_type)}"

        fn(typed_config)

def econf_foreach_hcm(econf, fn, chain_count=2):
    for listener in econf['static_resources']['listeners']:
        hcm_info = EnvoyHCMInfo

        econf_foreach_listener_chain(
            listener, fn, chain_count=chain_count,
            need_name=hcm_info.name, need_type=hcm_info.type)

def econf_foreach_cluster(econf, fn, name='cluster_httpbin_default'):
    for cluster in econf['static_resources']['clusters']:
        if cluster['name'] != name:
            continue

        found_cluster = True
        r = fn(cluster)
        if not r:
            break
    assert found_cluster

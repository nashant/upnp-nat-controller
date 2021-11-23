from kopf import Meta
from typing import Dict
from upnpy import UPnP
from kopf import Annotations


def advertise_proto(annotations: Dict[str, str], proto: str) -> bool:
    return annotations.get(f"{proto}.advertise.upnp/enabled", "false") == "true"


def advertise_tcp(meta: Meta, **_):
    return advertise_proto(meta.annotations, 'tcp')


def advertise_udp(meta: Meta, **_):
    return advertise_proto(meta.annotations, 'udp')


def get_svc():
    upnp = UPnP()
    if upnp.discover() == 0:
        return None
    device = upnp.get_igd()
    svcs = device.get_services()
    return next(filter(lambda s: 'AddPortMapping' in s.actions, svcs), None)


def proto_enabled(proto: str, annotations: Annotations):
    try:
        return annotations[f"{proto}.advertise.upnp/enabled"] == "true"
    except KeyError:
        return False


def get_ports(proto: str, annotations: Annotations) -> List[int]:
    if not proto_enabled(proto, annotations):
        return None
    try:
        return annotations[f"{proto}.advertise.upnp/ports"].split(",")
    except KeyError:
        return []
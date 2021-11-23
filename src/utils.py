from kopf import Meta
from typing import Dict
from upnpy import UPnP


def advertise_proto(annotations: Dict[str, str], proto: str) -> bool:
    return annotations.get(f"{proto}.advertise.upnp/enabled", "false") == "true"


def advertise_tcp(meta: Meta, **_):
    return advertise_proto(meta.annotations, 'tcp')


def advertise_udp(meta: Meta, **_):
    return advertise_proto(meta.annotations, 'udp')


def get_router():
    upnp = UPnP()
    devices = upnp.discover()
    device = upnp.get_igd()
    svcs = device.get_services()
    return next(filter(lambda s: 'AddPortMapping' in s.actions, svcs), None)
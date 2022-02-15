import kopf

from ipaddress import ip_address
from pydantic import BaseModel
from typing import Tuple, List, Union
from upnpy.ssdp.SSDPDevice import SSDPDevice
from upnpy.exceptions import SOAPError
from kopf import Annotations, Logger

from utils import get_ports


class LoadBalancer(BaseModel):
    ip: str
    svc: SSDPDevice.Service
    annotations: Annotations
    ports: List[Tuple[str,int]]

    class Config:
        arbitrary_types_allowed = True

    def __init__(self, **data):
        data["ports"] = []
        for proto in ["tcp", "udp"]:
            ports = get_ports(proto, data["annotations"])
            ports = [(proto.upper(), port) for port in ports]
            data["ports"].extend(ports)
        super().__init__(**data)

    def advertise(self, logger: Union[Logger, None]) -> None:
        unmapped = []
        for proto, port in self.ports:
            if logger is not None:
                logger.info(f"Adding port mapping for {self.ip}:{port}/{proto}")
            try:
                self.svc.AddPortMapping(
                    NewRemoteHost="",
                    NewExternalPort=port,
                    NewProtocol=proto,
                    NewInternalPort=port,
                    NewInternalClient=self.ip,
                    NewEnabled=1,
                    NewPortMappingDescription="",
                    NewLeaseDuration=0
                )
            except SOAPError as e:
                if e.error == 718:
                    logger.warn(f"Can't add port mapping, conflicting mapping exists")
                    unmapped.append((proto, port))
                else:
                    raise(e)
        if len(unmapped) > 0:
            unmapped_str = ", ".join(["/".join(port) for u in unmapped])
            kopf.TemporaryError(f"Unable to map ports: {unmapped_str}")

    def unadvertise(self, logger: Union[Logger, None]):
        for proto, port in self.ports:
            if logger is not None:
                logger.info(f"Deleting port mapping for {self.ip}:{port}/{proto}")

            self.svc.DeletePortMapping(
                NewRemoteHost="",
                NewExternalPort=port,
                NewProtocol=proto,
            )

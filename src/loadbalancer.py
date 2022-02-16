import kopf

from ipaddress import ip_address
from pydantic import BaseModel
from typing import Tuple, List, Union
from upnpy.ssdp.SSDPDevice import SSDPDevice
from upnpy.exceptions import SOAPError
from kopf import Annotations, Logger

from utils import get_ports


class LoadBalancer(BaseModel):
    name: str
    namespace: str
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

    def portmap_str(self, port: int, proto: str, ip: str=None):
        if ip is None:
            ip = self.ip
        return f"{ip}:{port}/{proto}"

    def advertise(self, logger: Logger) -> None:
        unmapped = []
        for proto, port in self.ports:
            logger.info(f"Adding port mapping for {self.portmap_str(port, proto)}")
            mapping = self.get_port_mapping(port, proto)
            if mapping is not None and mapping[1] != self.ip:
                logger.info(f"Existing mapping for {self.portmap_str(port, proto, mapping[1])}, {mapping[2]}")
                unmapped.append((proto, port))
                continue

            try:
                self.svc.AddPortMapping(
                    NewRemoteHost="",
                    NewExternalPort=port,
                    NewProtocol=proto,
                    NewInternalPort=port,
                    NewInternalClient=self.ip,
                    NewEnabled=1,
                    NewPortMappingDescription=f"{self.namespace}/{self.name}",
                    NewLeaseDuration=0
                )
            except SOAPError as e:
                if e.error == 718:
                    if mapping is None:
                        logger.warn(f"Unable to add mapping, port not allowed for mapping")
                    elif mapping[1] == self.ip:
                        continue
                    unmapped.append((proto, port))
                else:
                    raise(e)

        if len(unmapped) > 0:
            unmapped_str = ", ".join([f"{p[1]}/{p[0]}" for p in unmapped])
            raise(kopf.TemporaryError(f"Unable to map ports: {unmapped_str}"))

    def deadvertise(self, logger: Logger)-> None:
        for proto, port in self.ports:
            logger.info(f"Deleting port mapping for {self.portmap_str(port, proto)}")
            mapping = self.get_port_mapping(port, proto)
            if mapping is None:
                logger.info(f"Mapping for {self.portmap_str(port, proto)} doesn't exist")
                continue
            if mapping is not None and mapping[1] != self.ip:
                logger.info(f"Mapping not ours, {self.portmap_str(port, proto, mapping[1])}, {mapping[2]}")
                continue

            self.svc.DeletePortMapping(
                NewRemoteHost="",
                NewExternalPort=port,
                NewProtocol=proto,
            )

    def get_port_mapping(
        self,
        port: int,
        proto: str
    ) -> Union[Tuple[bool, str, str], None]:
        try:
            m = self.svc.GetSpecificPortMappingEntry(
                NewRemoteHost="",
                NewExternalPort=port,
                NewProtocol=proto
            )
            return (True, m["NewInternalClient"], m["NewPortMappingDescription"])
        except SOAPError as e:
            if e == 714:
                return None
            raise(e)
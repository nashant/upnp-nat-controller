import kopf


@kopf.on.startup()
def configure(settings: kopf.OperatorSettings, memo: kopf.Memo, **_):
    settings.persistence.finalizer = "advertise.upnp/kopf-finalizer"
    settings.persistence.progress_storage = kopf.AnnotationsProgressStorage(
        prefix="advertise.upnp"
    )
    settings.persistence.diffbase_storage = kopf.AnnotationsDiffBaseStorage(
        prefix="advertise.upnp",
        key="last-handled-configuration",
    )


import igd
import services

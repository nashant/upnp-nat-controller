FROM python:3.10-slim

EXPOSE 8080/tcp

ADD ./src /src
WORKDIR /src

ENV VIRTUAL_ENV=/src

RUN groupadd -g 1000 app \
 && useradd -mb /tmp -s /bin/bash -u 1000 -g 1000 app \
 && python3 -m venv $VIRTUAL_ENV \
 && pip install --upgrade pip \
 && pip install -r requirements.txt

ENV PATH="$VIRTUAL_ENV/bin:$PATH"

USER app

ENTRYPOINT [ "kopf", "run", "./controller.py"]
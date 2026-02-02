ARG BUILDIMAGE
FROM $BUILDIMAGE AS build

# BIRD build deps: flex, bison (parser), ncurses + readline (for birdc client)
RUN if [ ! -z "$(which apt)" ]; then \
       apt update && apt install -y build-essential curl \
        flex bison \
        libncurses5-dev libncurses-dev \
        libreadline-dev; \
    elif [ ! -z "$(which apk)" ]; then \
       apk add build-base curl \
        flex bison \
        linux-headers \
        ncurses-dev ncurses-static \
        readline-dev readline-static; \
    else \
       echo "unsupported package manager"; \
       exit 1; \
    fi

ARG VERSION
RUN curl --proto '=https' --tlsv1.2 -L "https://bird.network.cz/download/bird-$VERSION.tar.gz" \
	| tar -C / -zx

ARG BUILD_CFLAGS
ARG BUILD_LDFLAGS
ARG BUILD_CONFIGURE_FLAGS
RUN cd /bird-$VERSION \
	&& CFLAGS="${BUILD_CFLAGS}" LDFLAGS="${BUILD_LDFLAGS}" ./configure ${BUILD_CONFIGURE_FLAGS} \
	&& make -j$(nproc)

FROM scratch
ARG VERSION
COPY --from=build /bird-$VERSION/bird /bin/bird
COPY --from=build /bird-$VERSION/birdc /bin/birdc

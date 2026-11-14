BINARY := directcrate
DIST_DIR := dist
LDFLAGS := -s -w
ANDROID_JNI_DIR := android/app/src/main/jniLibs/arm64-v8a
GRADLE_APK := android/app/build/outputs/apk/release/app-release.apk

# Prefer JDK 17 paths over generic JAVA_HOME (which may point to Java 21/25 on CI runners)
JAVA_17_CANDIDATE := $(firstword $(wildcard \
	$(JAVA_HOME_17_X64) \
	/usr/lib/jvm/temurin-17-jdk-amd64 \
	/usr/lib/jvm/java-17-openjdk-amd64 \
	/home/linuxbrew/.linuxbrew/opt/openjdk@17 \
	/usr/lib/jvm/java-17-openjdk \
))

ifneq ($(JAVA_17_CANDIDATE),)
JAVA_HOME := $(JAVA_17_CANDIDATE)
endif

# Resolve Android SDK paths with fallbacks
ANDROID_HOME_CANDIDATE := $(firstword $(wildcard \
	$(ANDROID_HOME) \
	$(HOME)/Android/Sdk \
	/usr/local/lib/android/sdk \
))

ifneq ($(ANDROID_HOME_CANDIDATE),)
ANDROID_HOME := $(ANDROID_HOME_CANDIDATE)
endif

.PHONY: build build-all clean icons test test-all apk

icons:
	go run scripts/gen_resources.go

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test -v -race ./...

test-all:
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out

apk: icons
	@mkdir -p $(ANDROID_JNI_DIR) $(DIST_DIR)
	CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(ANDROID_JNI_DIR)/libdirectcrate.so .
	cd android && ANDROID_HOME="$(ANDROID_HOME)" JAVA_HOME="$(JAVA_HOME)" PATH="$(JAVA_HOME)/bin:$(PATH)" ./gradlew assembleRelease --no-daemon --stacktrace
	cp $(GRADLE_APK) $(DIST_DIR)/$(BINARY)-android-arm64.apk

build-all: icons apk
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-arm64 .
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-windows-amd64.exe .
	CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-android-arm64 .

clean:
	rm -rf $(DIST_DIR) $(BINARY) $(ANDROID_JNI_DIR) android/.gradle android/build android/app/build coverage.out

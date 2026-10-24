# Homebrew formula template. The release workflow fills VERSION and the
# SHA256 values and pushes it to the srawlin/homebrew-tap repository.
class Changeward < Formula
  desc "Change control for AI agents: preview, approve, apply and undo agent changes"
  homepage "https://github.com/srawlin/changeward"
  version "VERSION"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/srawlin/changeward/releases/download/vVERSION/changeward_darwin_arm64.tar.gz"
      sha256 "SHA256_DARWIN_ARM64"
    end
    on_intel do
      url "https://github.com/srawlin/changeward/releases/download/vVERSION/changeward_darwin_amd64.tar.gz"
      sha256 "SHA256_DARWIN_AMD64"
    end
  end
  on_linux do
    on_arm do
      url "https://github.com/srawlin/changeward/releases/download/vVERSION/changeward_linux_arm64.tar.gz"
      sha256 "SHA256_LINUX_ARM64"
    end
    on_intel do
      url "https://github.com/srawlin/changeward/releases/download/vVERSION/changeward_linux_amd64.tar.gz"
      sha256 "SHA256_LINUX_AMD64"
    end
  end

  def install
    bin.install "changeward"
  end

  test do
    assert_match "changeward", shell_output("#{bin}/changeward version")
  end
end

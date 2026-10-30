# Builds the speaker half. For the speaker:
#   make CXX=aarch64-linux-gnu-g++ LDFLAGS=-static
CXX ?= g++
CXXFLAGS ?= -std=c++17 -O2 -Wall -Wextra
LDFLAGS ?=

speaker-stats: agent.cpp ../../../sdk/cpp/marusya_plugin.hpp
	$(CXX) $(CXXFLAGS) -I../../../sdk/cpp -pthread agent.cpp -o $@ $(LDFLAGS)

clean:
	rm -f speaker-stats

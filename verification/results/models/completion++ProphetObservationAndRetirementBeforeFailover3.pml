// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go#L3
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_ProphetObservationAndRetirementBeforeFailover30 = [1] of {int}->
	run ProphetObservationAndRetirementBeforeFailover3(child_ProphetObservationAndRetirementBeforeFailover30)->
	child_ProphetObservationAndRetirementBeforeFailover30?0->
stop_process:skip
}

proctype ProphetObservationAndRetirementBeforeFailover3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousProphetObservationAndRetirementBeforeFailover962 = [1] of {int}->
	chan child_AnonymousProphetObservationAndRetirementBeforeFailover841 = [1] of {int}->
	chan child_AnonymousProphetObservationAndRetirementBeforeFailover740 = [1] of {int}->
	Chandef next_ch->
	Chandef retired_ch->
	Chandef observed_ch->
	run sync_monitor(observed_ch)->
	run sync_monitor(retired_ch)->
	run sync_monitor(next_ch)->
	run AnonymousProphetObservationAndRetirementBeforeFailover74(observed_ch,retired_ch,next_ch,child_AnonymousProphetObservationAndRetirementBeforeFailover740)->
	run receiver(child_AnonymousProphetObservationAndRetirementBeforeFailover740)->
	run AnonymousProphetObservationAndRetirementBeforeFailover84(observed_ch,retired_ch,next_ch,child_AnonymousProphetObservationAndRetirementBeforeFailover841)->
	run receiver(child_AnonymousProphetObservationAndRetirementBeforeFailover841)->
	run AnonymousProphetObservationAndRetirementBeforeFailover96(next_ch,observed_ch,retired_ch,child_AnonymousProphetObservationAndRetirementBeforeFailover962)->
	run receiver(child_AnonymousProphetObservationAndRetirementBeforeFailover962)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:10:2 */
	:: next_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:10:2 */->
	:: next_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:10:2 */ -> 
		next_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousProphetObservationAndRetirementBeforeFailover74(Chandef observed_ch;Chandef retired_ch;Chandef next_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:7:14 */
	:: observed_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:7:14 */
	:: observed_ch.sync!false -> 
		observed_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:7:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousProphetObservationAndRetirementBeforeFailover84(Chandef observed_ch;Chandef retired_ch;Chandef next_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:8:14 */
	:: retired_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:8:14 */
	:: retired_ch.sync!false -> 
		retired_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:8:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousProphetObservationAndRetirementBeforeFailover96(Chandef next_ch;Chandef observed_ch;Chandef retired_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:9:14 */
	:: observed_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:9:14 */->
	:: observed_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:9:14 */ -> 
		observed_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:9:26 */
	:: retired_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:9:26 */->
	:: retired_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:9:26 */ -> 
		retired_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:9:37 */
	:: next_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:9:37 */
	:: next_ch.sync!false -> 
		next_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet_route.go:9:37 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}


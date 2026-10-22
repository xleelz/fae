// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go#L6
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
	chan child_ProviderObservationBeforeDispatch60 = [1] of {int}->
	run ProviderObservationBeforeDispatch6(child_ProviderObservationBeforeDispatch60)->
	child_ProviderObservationBeforeDispatch60?0->
stop_process:skip
}

proctype ProviderObservationBeforeDispatch6(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_providerLaunch202 = [1] of {int}->
	chan child_providerSelect191 = [1] of {int}->
	chan child_providerPersist180 = [1] of {int}->
	Chandef done_ch->
	Chandef selected_ch->
	Chandef persisted_ch->
	Chandef observation_ch->
	run sync_monitor(observation_ch)->
	run sync_monitor(persisted_ch)->
	run sync_monitor(selected_ch)->
	run sync_monitor(done_ch)->
	run providerPersist18(observation_ch,persisted_ch,child_providerPersist180)->
	run receiver(child_providerPersist180)->
	run providerSelect19(persisted_ch,selected_ch,child_providerSelect191)->
	run receiver(child_providerSelect191)->
	run providerLaunch20(selected_ch,done_ch,child_providerLaunch202)->
	run receiver(child_providerLaunch202)->
	observation_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:14:2 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:14:2 */->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:15:2 */
	:: done_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:15:2 */->
	:: done_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:15:2 */ -> 
		done_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype providerPersist18(Chandef observation_ch;Chandef persisted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:18:57 */
	:: observation_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:18:57 */->
	:: observation_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:18:57 */ -> 
		observation_ch.rcving!false
	fi->
	persisted_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:18:72 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:18:72 */->
	stop_process: skip->
	child!0
}
proctype providerSelect19(Chandef persisted_ch;Chandef selected_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:19:57 */
	:: persisted_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:19:57 */->
	:: persisted_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:19:57 */ -> 
		persisted_ch.rcving!false
	fi->
	selected_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:19:70 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:19:70 */->
	stop_process: skip->
	child!0
}
proctype providerLaunch20(Chandef selected_ch;Chandef done_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:20:57 */
	:: selected_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:20:57 */->
	:: selected_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:20:57 */ -> 
		selected_ch.rcving!false
	fi->
	done_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:20:69 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/provider.go:20:69 */->
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

